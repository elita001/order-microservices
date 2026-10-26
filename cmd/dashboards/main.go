// Command dashboards generates the Grafana dashboards of OrderFlow with the Grafana
// Foundation SDK: `make dashboards` (or `go run ./cmd/dashboards`).
//
// Dashboards are code: this program is the source, and the JSON files in
// deploy/grafana/dashboards are its output, committed so that Grafana provisions them on
// startup. CI regenerates them and fails if the committed JSON differs.
// Panels are laid out left to right in rows of 24 grid columns.
//
// The SDK marks the classic dashboard schema (package dashboard) as deprecated in favor of
// the v2 schema, but Grafana 12 provisions classic JSON from files; v2 is behind a feature
// flag. .golangci.yml therefore ignores SA1019 in this package.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/grafana/grafana-foundation-sdk/go/bargauge"
	"github.com/grafana/grafana-foundation-sdk/go/cog"
	"github.com/grafana/grafana-foundation-sdk/go/common"
	"github.com/grafana/grafana-foundation-sdk/go/dashboard"
	"github.com/grafana/grafana-foundation-sdk/go/logs"
	"github.com/grafana/grafana-foundation-sdk/go/loki"
	"github.com/grafana/grafana-foundation-sdk/go/prometheus"
	"github.com/grafana/grafana-foundation-sdk/go/stat"
	"github.com/grafana/grafana-foundation-sdk/go/table"
	"github.com/grafana/grafana-foundation-sdk/go/timeseries"
)

func main() {
	out := flag.String("out", "deploy/grafana/dashboards", "directory for the dashboard JSON files")
	flag.Parse()

	dashboards := map[string]*dashboard.DashboardBuilder{
		"overview":  business(),
		"services":  services(),
		"messaging": messaging(),
		"logs":      logsDashboard(),
	}
	for name, builder := range dashboards {
		if err := write(filepath.Join(*out, name+".json"), builder); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	}
}

func write(path string, builder *dashboard.DashboardBuilder) error {
	d, err := builder.Build()
	if err != nil {
		return fmt.Errorf("build %s: %w", path, err)
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	// 0644: Grafana runs as another user (uid 472) and must be able to read the files.
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil { //nolint:gosec // public, non-secret files
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Printf("%s: %d panels\n", path, len(d.Panels))
	return nil
}

// ---------- shared settings ----------

const services6 = "gateway|order|customer|payment|delivery|notification"

// gRPC codes that mean the server failed (the same as in the GRPCServerErrors alert).
const grpcServerErrors = "Unknown|Internal|Unavailable|DeadlineExceeded|DataLoss"

var (
	promDS = common.DataSourceRef{Type: cog.ToPtr("prometheus"), Uid: cog.ToPtr("prometheus")}
	lokiDS = common.DataSourceRef{Type: cog.ToPtr("loki"), Uid: cog.ToPtr("loki")}
)

// Status colors are reserved for state (thresholds, up/down, breaker state).
const (
	good = "green"
	warn = "#EAB839"
	bad  = "red"
)

type step struct {
	color string
	from  *float64 // nil: from -infinity
}

func from(v float64) *float64 { return &v }

func thresholds(steps ...step) *dashboard.ThresholdsConfigBuilder {
	list := make([]dashboard.Threshold, 0, len(steps))
	for _, s := range steps {
		list = append(list, dashboard.Threshold{Color: s.color, Value: s.from})
	}
	return dashboard.NewThresholdsConfigBuilder().Mode(dashboard.ThresholdsModeAbsolute).Steps(list)
}

func valueMap(options map[string][2]string) []dashboard.ValueMapping {
	results := make(map[string]dashboard.ValueMappingResult, len(options))
	for value, textAndColor := range options {
		results[value] = dashboard.ValueMappingResult{Text: cog.ToPtr(textAndColor[0]), Color: cog.ToPtr(textAndColor[1])}
	}
	return []dashboard.ValueMapping{{ValueMap: &dashboard.ValueMap{Type: dashboard.MappingTypeValueToText, Options: results}}}
}

// refID names the n-th query of a panel: A, B, C... Grafana tells the queries of a panel
// apart by it; without it, the results of several queries are mixed up.
func refID(n int) string { return string(rune('A' + n)) }

func promQuery(expr, legend string) *prometheus.DataqueryBuilder {
	if legend == "" {
		legend = "__auto"
	}
	return prometheus.NewDataqueryBuilder().RefId(refID(0)).Datasource(promDS).Expr(expr).LegendFormat(legend)
}

func lokiQuery(expr, legend string) *loki.DataqueryBuilder {
	if legend == "" {
		legend = "__auto"
	}
	return loki.NewDataqueryBuilder().RefId(refID(0)).Datasource(lokiDS).Expr(expr).LegendFormat(legend)
}

func reduceLast() *common.ReduceDataOptionsBuilder {
	return common.NewReduceDataOptionsBuilder().Calcs([]string{"lastNotNull"}).Fields("").Values(false)
}

// statPanel is a headline number; with steps its background shows the state.
// With a legend, every series is shown with its name.
func statPanel(title, expr, legend, unit string, w, h uint32) *stat.PanelBuilder {
	textMode := common.BigValueTextModeValue
	if legend != "" {
		textMode = common.BigValueTextModeValueAndName
	}
	return stat.NewPanelBuilder().
		Title(title).Datasource(promDS).Span(w).Height(h).Unit(unit).
		WithTarget(promQuery(expr, legend).Instant()).
		ReduceOptions(reduceLast()).
		ColorScheme(dashboard.NewFieldColorBuilder().Mode(dashboard.FieldColorModeIdFixed).FixedColor("text")).
		ColorMode(common.BigValueColorModeNone).
		GraphMode(common.BigValueGraphModeNone).
		TextMode(textMode).
		JustifyMode(common.BigValueJustifyModeCenter).
		Orientation(common.VizOrientationAuto).
		WideLayout(true)
}

// withState colors the stat background by thresholds.
func withState(p *stat.PanelBuilder, steps ...step) *stat.PanelBuilder {
	return p.Thresholds(thresholds(steps...)).
		ColorScheme(dashboard.NewFieldColorBuilder().Mode(dashboard.FieldColorModeIdThresholds)).
		ColorMode(common.BigValueColorModeBackground)
}

type series struct{ expr, legend string }

func timeseriesPanel(title, unit string, w uint32, queries ...series) *timeseries.PanelBuilder {
	p := timeseries.NewPanelBuilder().
		Title(title).Datasource(promDS).Span(w).Height(8).Unit(unit).Min(0).
		ColorScheme(dashboard.NewFieldColorBuilder().Mode(dashboard.FieldColorModeIdPaletteClassic)).
		DrawStyle(common.GraphDrawStyleLine).LineWidth(2).FillOpacity(0).
		ShowPoints(common.VisibilityModeNever).
		SpanNulls(common.BoolOrFloat64{Bool: cog.ToPtr(true)}).
		Legend(common.NewVizLegendOptionsBuilder().DisplayMode(common.LegendDisplayModeList).
			Placement(common.LegendPlacementBottom).ShowLegend(true)).
		Tooltip(common.NewVizTooltipOptionsBuilder().Mode(common.TooltipDisplayModeMulti).Sort(common.SortOrderDescending))
	if unit == "percentunit" {
		p = p.Max(1) // a ratio gets a fixed 0..100% axis
	}
	for i, q := range queries {
		p = p.WithTarget(promQuery(q.expr, q.legend).RefId(refID(i)))
	}
	return p
}

// withLimit draws a red threshold line and area above limit.
func withLimit(p *timeseries.PanelBuilder, limit float64) *timeseries.PanelBuilder {
	return p.Thresholds(thresholds(step{"transparent", nil}, step{bad, from(limit)})).
		ThresholdsStyle(common.NewGraphThresholdsStyleConfigBuilder().Mode(common.GraphThresholdsStyleModeLineAndArea))
}

func barsPanel(title, expr, legend string) *bargauge.PanelBuilder {
	return bargauge.NewPanelBuilder().
		Title(title).Datasource(promDS).Span(8).Height(8).Unit("short").Min(0).
		WithTarget(promQuery(expr, legend).Instant()).
		ColorScheme(dashboard.NewFieldColorBuilder().Mode(dashboard.FieldColorModeIdPaletteClassic)).
		Thresholds(thresholds(step{"text", nil})).
		Orientation(common.VizOrientationHorizontal).
		DisplayMode(common.BarGaugeDisplayModeBasic).ShowUnfilled(true).
		ValueMode(common.BarGaugeValueModeText).NamePlacement(common.BarGaugeNamePlacementLeft).
		Sizing(common.BarGaugeSizingAuto).MinVizHeight(16).MaxVizHeight(28).
		Text(common.NewVizTextDisplayOptionsBuilder().TitleSize(14).ValueSize(18)).
		ReduceOptions(reduceLast())
}

// countsTable shows the counts over the selected time range, one row per label set,
// largest first.
func countsTable(title, expr string) *table.PanelBuilder {
	return table.NewPanelBuilder().
		Title(title).Datasource(promDS).Span(24).Height(8).Unit("short").
		WithTarget(promQuery(expr, "").Instant().Format(prometheus.PromQueryFormatTable)).
		WithTransformation(dashboard.DataTransformerConfig{Id: "organize", Options: map[string]any{"excludeByName": map[string]bool{"Time": true}}}).
		ShowHeader(true).
		SortBy([]cog.Builder[common.TableSortByFieldState]{common.NewTableSortByFieldStateBuilder().DisplayName("Value").Desc(true)})
}

func links() []cog.Builder[dashboard.DashboardLink] {
	var list []cog.Builder[dashboard.DashboardLink]
	for _, l := range [][2]string{{"Business", "orderflow-business"}, {"Services", "orderflow-services"},
		{"Messaging", "orderflow-messaging"}, {"Logs", "orderflow-logs"}} {
		list = append(list, dashboard.NewDashboardLinkBuilder(l[0]).Type(dashboard.DashboardLinkTypeLink).
			Url("/d/"+l[1]).Icon("dashboard"))
	}
	return list
}

func newDashboard(uid, title, description string) *dashboard.DashboardBuilder {
	return dashboard.NewDashboardBuilder(title).
		Uid(uid).Description(description).Tags([]string{"orderflow"}).
		Timezone("browser").Readonly().Tooltip(dashboard.DashboardCursorSyncCrosshair).
		Refresh("10s").Time("now-30m", "now").
		Links(links())
}

// Counts over a range use increase(), which extrapolates to the range edges.
const approx = " Approximate: increase() extrapolates to the range edges; exact numbers come from the database."

// ---------- Business ----------

func business() *dashboard.DashboardBuilder {
	return newDashboard("orderflow-business", "OrderFlow - Business", "Orders, payments, shipments, and customer emails.").
		WithPanel(statPanel("Orders", `round(sum(increase(orders_created_total[$__range])))`, "", "short", 6, 4).Decimals(0).
			Description("Orders created in the selected time range (idempotent replays are not counted)." + approx)).
		WithPanel(withState(statPanel("Payment success rate",
			`sum(increase(payments_total{status="succeeded"}[$__range])) / sum(increase(payments_total[$__range]))`, "", "percentunit", 6, 4).Decimals(1),
			step{bad, nil}, step{warn, from(0.7)}, step{good, from(0.9)})).
		WithPanel(stat.NewPanelBuilder().
			Title("Revenue").Datasource(promDS).Span(6).Height(4).Unit("short").Decimals(2).
			Description("Successful payments in the selected range, in major units, per currency." + approx).
			WithTarget(promQuery(`sum by (currency) (increase(payment_amount_minor_units_total{status="succeeded"}[$__range])) / 100`, "{{currency}}").Instant()).
			ReduceOptions(reduceLast()).
			ColorScheme(dashboard.NewFieldColorBuilder().Mode(dashboard.FieldColorModeIdFixed).FixedColor("text")).
			ColorMode(common.BigValueColorModeNone).GraphMode(common.BigValueGraphModeNone).
			TextMode(common.BigValueTextModeValueAndName).JustifyMode(common.BigValueJustifyModeCenter).
			Orientation(common.VizOrientationAuto)).
		WithPanel(statPanel("Orders shipped", `round(sum(increase(order_status_changes_total{status="shipped"}[$__range])))`, "", "short", 6, 4).
			Decimals(0).Description(approx[1:])).
		WithPanel(timeseriesPanel("Orders per minute", "short", 12, series{`sum(rate(orders_created_total[1m])) * 60`, "orders"})).
		WithPanel(timeseriesPanel("Payments per minute", "short", 12, series{`sum by (status) (rate(payments_total[1m])) * 60`, "{{status}}"})).
		WithPanel(barsPanel("Decline reasons", `round(sum by (reason) (increase(payments_total{status="failed"}[$__range])))`, "{{reason}}")).
		WithPanel(barsPanel("Shipments by carrier", `round(sum by (carrier) (increase(shipments_registered_total[$__range])))`, "{{carrier}}")).
		WithPanel(barsPanel("Emails sent", `round(sum by (kind) (increase(notifications_sent_total[$__range])))`, "{{kind}}")).
		WithPanel(timeseriesPanel("Order status changes per minute", "short", 24,
			series{`sum by (status) (rate(order_status_changes_total[1m])) * 60`, "{{status}}"}))
}

// ---------- Services ----------

func services() *dashboard.DashboardBuilder {
	routeLatency := func(quantile, name string) series {
		return series{
			fmt.Sprintf(`histogram_quantile(%s, sum by (le, method, route) (rate(http_request_duration_seconds_bucket{job="gateway", route!="unmatched"}[5m])))`, quantile),
			name + " {{method}} {{route}}",
		}
	}
	p50, p95, p99 := routeLatency("0.50", "p50"), routeLatency("0.95", "p95"), routeLatency("0.99", "p99")

	return newDashboard("orderflow-services", "OrderFlow - Services",
		"RED metrics of every service, the gateway, the cache, and circuit breakers.").
		WithPanel(withState(statPanel("Service health", `up{job=~"`+services6+`"}`, "{{job}}", "short", 24, 3),
			step{bad, nil}, step{good, from(1)}).
			Mappings(valueMap(map[string][2]string{"0": {"DOWN", bad}, "1": {"UP", good}}))).
		WithRow(dashboard.NewRowBuilder("RED: rate, errors, duration")).
		WithPanel(timeseriesPanel("Request rate", "reqps", 8, series{`sum by (job) (rate(http_requests_total[1m]))`, "{{job}}"})).
		WithPanel(withLimit(timeseriesPanel("Error ratio (5xx)", "percentunit", 8, series{
			// "(errors or total * 0) / total": without 5xx the numerator is empty, and the panel
			// would show "No data" instead of 0%.
			`(sum by (job) (rate(http_requests_total{status=~"5.."}[5m])) or sum by (job) (rate(http_requests_total[5m])) * 0)` +
				` / sum by (job) (rate(http_requests_total[5m]))`, "{{job}}",
		}), 0.05).Description("Share of 5xx responses. The HighErrorRate alert fires above 5%.")).
		WithPanel(withLimit(timeseriesPanel("Latency p95", "s", 8,
			series{`histogram_quantile(0.95, sum by (le, job) (rate(http_request_duration_seconds_bucket[5m])))`, "{{job}}"}), 1)).
		WithPanel(timeseriesPanel("Gateway latency by route (p50 / p95 / p99)", "s", 12, p50, p95, p99)).
		WithPanel(timeseriesPanel("Gateway responses by status", "reqps", 12,
			series{`sum by (status) (rate(http_requests_total{job="gateway"}[1m]))`, "{{status}}"}).
			Description("429: rate limited, 502/504: upstream unavailable or too slow, 503: database down.")).
		WithPanel(countsTable("Requests by route (selected range)",
			`round(sum by (job, method, route, status) (increase(http_requests_total{job=~"`+services6+`"}[$__range]))) > 0`)).
		WithRow(dashboard.NewRowBuilder("gRPC: order and customer services")).
		WithPanel(timeseriesPanel("gRPC calls by method and code", "reqps", 8,
			series{`sum by (grpc_method, grpc_code) (rate(grpc_server_handled_total{grpc_service!="grpc.health.v1.Health"}[1m]))`, "{{grpc_method}} {{grpc_code}}"}).
			Description("Calls the servers handled. NotFound, InvalidArgument, PermissionDenied are answers to bad calls, not failures.")).
		WithPanel(withLimit(timeseriesPanel("gRPC server error ratio", "percentunit", 8, series{
			`(sum by (job) (rate(grpc_server_handled_total{grpc_code=~"` + grpcServerErrors + `"}[5m])) or sum by (job) (rate(grpc_server_handled_total[5m])) * 0)` +
				` / sum by (job) (rate(grpc_server_handled_total[5m]))`, "{{job}}",
		}), 0.05).Description("Share of calls failed by the server (" + grpcServerErrors + "). The GRPCServerErrors alert fires above 5%.")).
		WithPanel(withLimit(timeseriesPanel("gRPC latency p95: server and client", "s", 8,
			series{`histogram_quantile(0.95, sum by (le, job, grpc_method) (rate(grpc_server_handling_seconds_bucket{grpc_service!="grpc.health.v1.Health"}[5m])))`, "{{job}} serves {{grpc_method}}"},
			series{`histogram_quantile(0.95, sum by (le, job, grpc_method) (rate(grpc_client_handling_seconds_bucket[5m])))`, "{{job}} calls {{grpc_method}}"}), 1).
			Description("The client side includes the network, retries, and waiting for a connection.")).
		WithPanel(timeseriesPanel("gRPC calls by caller", "reqps", 12,
			series{`sum by (job, grpc_method, grpc_code) (rate(grpc_client_handled_total[1m]))`, "{{job}} → {{grpc_method}} {{grpc_code}}"}).
			Description("Unavailable without a server-side error: the service could not be reached (the gateway answers 502).")).
		WithPanel(timeseriesPanel("Customer profile lookups", "ops", 12,
			series{`sum by (result) (rate(customer_lookups_total[5m]))`, "{{result}}"}).
			Description("fresh: local copy; refreshed: from Keycloak; stale: Keycloak failed, an old copy was served; failed: no copy to fall back to.")).
		WithPanel(countsTable("gRPC calls by method (selected range)",
			`round(sum by (job, grpc_method, grpc_code) (increase(grpc_server_handled_total{grpc_service!="grpc.health.v1.Health"}[$__range]))) > 0`).
			Description("How many calls each server handled in the selected time range: single calls are easier to see here than on the rate graphs.")).
		WithRow(dashboard.NewRowBuilder("Gateway, cache, circuit breakers")).
		WithPanel(timeseriesPanel("Rate limiter decisions", "reqps", 8,
			series{`sum by (policy, decision) (rate(gateway_rate_limit_decisions_total[1m]))`, "{{policy}} {{decision}}"}).
			Description("bypassed: Redis unavailable, requests pass without a limit (fail-open).")).
		WithPanel(timeseriesPanel("Idempotency-Key requests", "reqps", 8,
			series{`sum by (result) (rate(gateway_idempotency_requests_total[1m]))`, "{{result}}"}).
			Description("replayed: the stored response was returned; bypassed: Redis unavailable, the order service deduplicates.")).
		WithPanel(timeseriesPanel("Order cache hit ratio", "percentunit", 8,
			series{`sum(rate(order_cache_requests_total{result="hit"}[5m])) / sum(rate(order_cache_requests_total[5m]))`, "hit ratio"})).
		WithPanel(withState(statPanel("Circuit breakers", "circuit_breaker_state", "{{job}} · {{name}}", "short", 24, 4),
			step{good, nil}, step{warn, from(1)}, step{bad, from(2)}).
			Mappings(valueMap(map[string][2]string{"0": {"closed", good}, "1": {"half-open", warn}, "2": {"OPEN", bad}})).
			Description("Open: calls to the dependency are suspended and fail fast."))
}

// ---------- Messaging ----------

func messaging() *dashboard.DashboardBuilder {
	return newDashboard("orderflow-messaging", "OrderFlow - Messaging",
		"Consumers, retries, dead letter queues, RabbitMQ queue depth, and the transactional outbox.").
		WithPanel(withState(statPanel("Dead-lettered messages", `sum(rabbitmq_detailed_queue_messages{queue=~".*[.]dlq"}) or vector(0)`, "", "short", 6, 4).Decimals(0),
			step{good, nil}, step{bad, from(1)}).
			Description("Messages that failed permanently. Inspect with make dlq-list queue=<service queue>.")).
		WithPanel(withState(statPanel("Outbox lag", "max(outbox_oldest_pending_age_seconds)", "", "s", 6, 4).Decimals(0),
			step{good, nil}, step{warn, from(10)}, step{bad, from(60)}).
			Description("Age of the oldest unpublished event across services.")).
		WithPanel(withState(statPanel("Paused consumers", "sum(messaging_consumer_paused)", "", "short", 6, 4).Decimals(0),
			step{good, nil}, step{bad, from(1)}).
			Description("Consumers wait while their database is unavailable.")).
		WithPanel(withState(statPanel("Retries per minute", `sum(rate(messaging_consumed_messages_total{result="retry"}[5m])) * 60 or vector(0)`, "", "short", 6, 4).Decimals(1),
			step{good, nil}, step{warn, from(1)}, step{bad, from(10)})).
		WithRow(dashboard.NewRowBuilder("Consumers")).
		WithPanel(timeseriesPanel("Processed messages by result", "ops", 12,
			series{`sum by (queue, result) (rate(messaging_consumed_messages_total[1m]))`, "{{queue}} {{result}}"}).
			Description("ack: processed, retry: scheduled for a retry, dead_letter: moved to the DLQ.")).
		WithPanel(timeseriesPanel("Handler duration p95", "s", 12,
			series{`histogram_quantile(0.95, sum by (le, queue) (rate(messaging_processing_duration_seconds_bucket[5m])))`, "{{queue}}"})).
		WithRow(dashboard.NewRowBuilder("Queues (RabbitMQ)")).
		WithPanel(timeseriesPanel("Service queues", "short", 8,
			series{`sum by (queue) (rabbitmq_detailed_queue_messages{queue=~".*-service"})`, "{{queue}}"}).
			Description("Messages waiting to be consumed.")).
		WithPanel(timeseriesPanel("Waiting for a retry", "short", 8, series{
			`sum by (service) (label_replace(rabbitmq_detailed_queue_messages{queue=~".*[.]retry[.].*"}, "service", "$1", "queue", "(.*)[.]retry[.].*"))`,
			"{{service}}",
		}).Description("Messages in the retry queues (<queue>.retry.<delay>), summed per service.")).
		WithPanel(withLimit(timeseriesPanel("Dead letter queues", "short", 8,
			series{`sum by (queue) (rabbitmq_detailed_queue_messages{queue=~".*[.]dlq"})`, "{{queue}}"}), 1)).
		WithRow(dashboard.NewRowBuilder("Transactional outbox")).
		WithPanel(timeseriesPanel("Published events", "ops", 8, series{`sum by (table) (rate(outbox_published_messages_total[1m]))`, "{{table}}"})).
		WithPanel(timeseriesPanel("Pending events", "short", 8, series{"outbox_pending_messages", "{{table}}"})).
		WithPanel(withLimit(timeseriesPanel("Oldest pending event", "s", 8, series{"outbox_oldest_pending_age_seconds", "{{table}}"}), 60).
			Description("Grows while the broker is unavailable. The OutboxLagging alert fires above 60s."))
}

// ---------- Logs (Loki) ----------

func logsDashboard() *dashboard.DashboardBuilder {
	lokiTimeseries := func(title, expr, legend string) *timeseries.PanelBuilder {
		return timeseriesPanel(title, "short", 12).Datasource(lokiDS).WithTarget(lokiQuery(expr, legend))
	}
	return newDashboard("orderflow-logs", "OrderFlow - Logs",
		"Logs of all services in Loki, filterable by request ID, with links to traces in Tempo.").
		WithVariable(dashboard.NewCustomVariableBuilder("service").Label("Service").
			Values(dashboard.StringOrMap{String: cog.ToPtr("gateway,order,customer,payment,delivery,notification")}).
			Multi(true).IncludeAll(true).AllValue(services6).
			Current(dashboard.VariableOption{
				Selected: cog.ToPtr(true),
				Text:     dashboard.StringOrArrayOfString{ArrayOfString: []string{"All"}},
				Value:    dashboard.StringOrArrayOfString{ArrayOfString: []string{"$__all"}},
			})).
		WithVariable(dashboard.NewTextBoxVariableBuilder("request_id").Label("Request ID")).
		WithPanel(lokiTimeseries("Log lines by level",
			`sum by (level) (count_over_time({service=~"$service", level=~".+"}[1m]))`, "{{level}}")).
		WithPanel(lokiTimeseries("Errors by service",
			`sum by (service) (count_over_time({service=~"$service", level="ERROR"}[1m]))`, "{{service}}").
			Description("ERROR records per minute. Find the cause in the logs below by request_id or open the trace.")).
		WithPanel(logs.NewPanelBuilder().
			Title("Logs").Datasource(lokiDS).Span(24).Height(16).
			Description("Enter a request ID above to see one order's whole flow across all services. " +
				"Expand a record and use the trace_id link to open its trace in Tempo.").
			WithTarget(lokiQuery(`{service=~"$service"} | request_id=~"$request_id.*"`, "").QueryType("range")).
			ShowTime(true).WrapLogMessage(true).SortOrder(common.LogsSortOrderDescending).
			EnableLogDetails(true).DedupStrategy(common.LogsDedupStrategyNone).PrettifyLogMessage(false))
}
