package loki

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/grafana/dskit/concurrency"
	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/datasource"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/grafana/grafana-plugin-sdk-go/backend/httpclient"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/infra/tracing"
	"github.com/grafana/grafana/pkg/services/featuremgmt"
	ngalertmodels "github.com/grafana/grafana/pkg/services/ngalert/models"
	"github.com/grafana/grafana/pkg/tsdb/loki/kinds/dataquery"
)

var logger = log.New("tsdb.loki")

type Service struct {
	im       instancemgmt.InstanceManager
	features featuremgmt.FeatureToggles
	tracer   tracing.Tracer
	logger   *log.ConcreteLogger
}

var (
	_ backend.QueryDataHandler    = (*Service)(nil)
	_ backend.StreamHandler       = (*Service)(nil)
	_ backend.CallResourceHandler = (*Service)(nil)
)

func ProvideService(httpClientProvider *httpclient.Provider, features featuremgmt.FeatureToggles, tracer tracing.Tracer) *Service {
	return &Service{
		im:       datasource.NewInstanceManager(newInstanceSettings(httpClientProvider)),
		features: features,
		tracer:   tracer,
		logger:   logger,
	}
}

var (
	legendFormat = regexp.MustCompile(`\{\{\s*(.+?)\s*\}\}`)
)

// Used in logging to mark a stage
var (
	stagePrepareRequest  = "prepareRequest"
	stageDatabaseRequest = "databaseRequest"
	stageParseResponse   = "parseResponse"
)

type datasourceInfo struct {
	HTTPClient *http.Client
	URL        string

	// open streams
	streams   map[string]data.FrameJSONCache
	streamsMu sync.RWMutex
}

type QueryJSONModel struct {
	dataquery.LokiDataQuery
	Direction           *string `json:"direction,omitempty"`
	SupportingQueryType *string `json:"supportingQueryType"`
}

type ResponseOpts struct {
	metricDataplane bool
	logsDataplane   bool
}

func parseQueryModel(raw json.RawMessage) (*QueryJSONModel, error) {
	model := &QueryJSONModel{}
	err := json.Unmarshal(raw, model)
	return model, err
}

func newInstanceSettings(httpClientProvider *httpclient.Provider) datasource.InstanceFactoryFunc {
	return func(ctx context.Context, settings backend.DataSourceInstanceSettings) (instancemgmt.Instance, error) {
		opts, err := settings.HTTPClientOptions(ctx)
		if err != nil {
			return nil, err
		}

		client, err := httpClientProvider.New(opts)
		if err != nil {
			return nil, err
		}

		model := &datasourceInfo{
			HTTPClient: client,
			URL:        settings.URL,
			streams:    make(map[string]data.FrameJSONCache),
		}
		return model, nil
	}
}

func (s *Service) CallResource(ctx context.Context, req *backend.CallResourceRequest, sender backend.CallResourceResponseSender) error {
	dsInfo, err := s.getDSInfo(ctx, req.PluginContext)
	logger := s.logger.FromContext(ctx)
	if err != nil {
		logger.Error("Failed to get data source info", "error", err)
		return err
	}
	return callResource(ctx, req, sender, dsInfo, logger, s.tracer)
}

func callResource(ctx context.Context, req *backend.CallResourceRequest, sender backend.CallResourceResponseSender, dsInfo *datasourceInfo, plog log.Logger, tracer tracing.Tracer) error {
	url := req.URL
	// a very basic is-this-url-valid check
	if req.Method != "GET" {
		plog.Error("Invalid HTTP method", "method", req.Method)
		return fmt.Errorf("invalid HTTP method: %s", req.Method)
	}
	if (!strings.HasPrefix(url, "labels?")) &&
		(!strings.HasPrefix(url, "label/")) && // the `/label/$label_name/values` form
		(!strings.HasPrefix(url, "series?")) &&
		(!strings.HasPrefix(url, "index/stats?")) {
		plog.Error("Invalid URL", "url", url)
		return fmt.Errorf("invalid URL: %s", url)
	}
	lokiURL := fmt.Sprintf("/loki/api/v1/%s", url)

	ctx, span := tracer.Start(ctx, "datasource.loki.CallResource", trace.WithAttributes(
		attribute.String("url", lokiURL),
	))
	defer span.End()

	api := newLokiAPI(dsInfo.HTTPClient, dsInfo.URL, plog, tracer, false)

	rawLokiResponse, err := api.RawQuery(ctx, lokiURL)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		plog.Error("Failed resource call from loki", "err", err, "url", lokiURL)
		return err
	}
	respHeaders := map[string][]string{
		"content-type": {"application/json"},
	}
	if rawLokiResponse.Encoding != "" {
		respHeaders["content-encoding"] = []string{rawLokiResponse.Encoding}
	}
	return sender.Send(&backend.CallResourceResponse{
		Status:  rawLokiResponse.Status,
		Headers: respHeaders,
		Body:    rawLokiResponse.Body,
	})
}

func (s *Service) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
	dsInfo, err := s.getDSInfo(ctx, req.PluginContext)
	_, fromAlert := req.Headers[ngalertmodels.FromAlertHeaderName]
	logger := logger.FromContext(ctx).New("fromAlert", fromAlert)
	if err != nil {
		logger.Error("Failed to get data source info", "err", err)
		result := backend.NewQueryDataResponse()
		return result, err
	}

	responseOpts := ResponseOpts{
		metricDataplane: s.features.IsEnabled(ctx, featuremgmt.FlagLokiMetricDataplane),
		logsDataplane:   s.features.IsEnabled(ctx, featuremgmt.FlagLokiLogsDataplane),
	}

	return queryData(ctx, req, dsInfo, responseOpts, s.tracer, logger, s.features.IsEnabled(ctx, featuremgmt.FlagLokiRunQueriesInParallel), s.features.IsEnabled(ctx, featuremgmt.FlagLokiStructuredMetadata))
}

func queryData(ctx context.Context, req *backend.QueryDataRequest, dsInfo *datasourceInfo, responseOpts ResponseOpts, tracer tracing.Tracer, plog log.Logger, runInParallel bool, requestStructuredMetadata bool) (*backend.QueryDataResponse, error) {
	result := backend.NewQueryDataResponse()

	api := newLokiAPI(dsInfo.HTTPClient, dsInfo.URL, plog, tracer, requestStructuredMetadata)

	start := time.Now()
	queries, err := parseQuery(req)
	if err != nil {
		plog.Error("Failed to prepare request to Loki", "error", err, "duration", time.Since(start), "queriesLength", len(queries), "stage", stagePrepareRequest)
		return result, err
	}

	plog.Info("Prepared request to Loki", "duration", time.Since(start), "queriesLength", len(queries), "stage", stagePrepareRequest, "runInParallel", runInParallel)

	ctx, span := tracer.Start(ctx, "datasource.loki.queryData.runQueries", trace.WithAttributes(
		attribute.Bool("runInParallel", runInParallel),
		attribute.Int("queriesLength", len(queries)),
	))
	if req.GetHTTPHeader("X-Query-Group-Id") != "" {
		span.SetAttributes(attribute.String("query_group_id", req.GetHTTPHeader("X-Query-Group-Id")))
	}
	defer span.End()
	start = time.Now()

	// We are testing running of queries in parallel behind feature flag
	if runInParallel {
		resultLock := sync.Mutex{}
		err = concurrency.ForEachJob(ctx, len(queries), 10, func(ctx context.Context, idx int) error {
			query := queries[idx]
			queryRes := executeQuery(ctx, query, req, runInParallel, api, responseOpts, tracer, plog)

			resultLock.Lock()
			defer resultLock.Unlock()
			result.Responses[query.RefID] = queryRes
			return nil // errors are saved per-query,always return nil
		})
	} else {
		for _, query := range queries {
			queryRes := executeQuery(ctx, query, req, runInParallel, api, responseOpts, tracer, plog)
			result.Responses[query.RefID] = queryRes
		}
	}
	plog.Debug("Executed queries", "duration", time.Since(start), "queriesLength", len(queries), "runInParallel", runInParallel)
	return result, err
}

func executeQuery(ctx context.Context, query *lokiQuery, req *backend.QueryDataRequest, runInParallel bool, api *LokiAPI, responseOpts ResponseOpts, tracer tracing.Tracer, plog log.Logger) backend.DataResponse {
	ctx, span := tracer.Start(ctx, "datasource.loki.queryData.runQueries.runQuery", trace.WithAttributes(
		attribute.Bool("runInParallel", runInParallel),
		attribute.String("expr", query.Expr),
		attribute.Int64("start_unixnano", query.Start.UnixNano()),
		attribute.Int64("stop_unixnano", query.End.UnixNano()),
	))
	if req.GetHTTPHeader("X-Query-Group-Id") != "" {
		span.SetAttributes(attribute.String("query_group_id", req.GetHTTPHeader("X-Query-Group-Id")))
	}

	defer span.End()

	queryRes, err := runQuery(ctx, api, query, responseOpts, plog)
	if queryRes == nil {
		// we always want to return a backend.DataResponse object, even if we received just an error
		queryRes = &backend.DataResponse{}
	}

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		queryRes.Error = err
	}

	return *queryRes
}

// we extracted this part of the functionality to make it easy to unit-test it
func runQuery(ctx context.Context, api *LokiAPI, query *lokiQuery, responseOpts ResponseOpts, plog log.Logger) (*backend.DataResponse, error) {
	res, err := api.DataQuery(ctx, *query, responseOpts)
	if err != nil {
		plog.Error("Error querying loki", "error", err)
		return res, err
	}

	for _, frame := range res.Frames {
		err = adjustFrame(frame, query, !responseOpts.metricDataplane, responseOpts.logsDataplane)

		if err != nil {
			plog.Error("Error adjusting frame", "error", err)
			return res, err
		}
	}

	return res, nil
}

func (s *Service) getDSInfo(ctx context.Context, pluginCtx backend.PluginContext) (*datasourceInfo, error) {
	i, err := s.im.Get(ctx, pluginCtx)
	if err != nil {
		return nil, err
	}

	instance, ok := i.(*datasourceInfo)
	if !ok {
		return nil, fmt.Errorf("failed to cast data source info")
	}

	return instance, nil
}
