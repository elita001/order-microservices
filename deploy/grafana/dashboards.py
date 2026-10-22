"""Generates the OrderFlow Grafana dashboards: `make dashboards`.

Dashboards are code: this script is the source, and the JSON files in ./dashboards are its
output, committed so that Grafana provisions them on startup. Edit the panels here, run
`make dashboards`, and restart Grafana (`docker compose restart grafana`).
Panels are laid out left to right in rows of 24 grid columns.
"""
import json
import pathlib

OUT = pathlib.Path(__file__).resolve().parent / "dashboards"
DS = {"type": "prometheus", "uid": "prometheus"}
LOKI = {"type": "loki", "uid": "loki"}
SERVICES = "gateway|order|payment|delivery|notification"

# Status colors are reserved for state (thresholds, up/down, breaker state).
GOOD, WARN, BAD = "green", "#EAB839", "red"


def target(expr, legend="", instant=False, ref="A", ds=DS):
    t = {"refId": ref, "datasource": ds, "expr": expr, "legendFormat": legend or "__auto"}
    if instant:
        t.update(instant=True, range=False)
    return t


def thresholds(*steps):
    """steps: (color, from_value) pairs; the first one starts at -inf."""
    return {"mode": "absolute", "steps": [{"color": c, "value": v} for c, v in steps]}


class Builder:
    def __init__(self):
        self.panels, self.x, self.y, self.row_h, self.id = [], 0, 0, 0, 1

    def add(self, panel, w, h, ds=DS):
        if self.x + w > 24:
            self.x, self.y, self.row_h = 0, self.y + self.row_h, 0
        panel.update(id=self.id, gridPos={"h": h, "w": w, "x": self.x, "y": self.y}, datasource=ds)
        self.id += 1
        self.x += w
        self.row_h = max(self.row_h, h)
        self.panels.append(panel)

    def row(self, title):
        self.x, self.y, self.row_h = 0, self.y + self.row_h, 0
        self.panels.append({"id": self.id, "type": "row", "title": title, "collapsed": False,
                            "gridPos": {"h": 1, "w": 24, "x": 0, "y": self.y}, "panels": []})
        self.id += 1
        self.y += 1

    def stat(self, title, expr, unit="short", w=6, h=4, steps=None, decimals=None, legend="", mappings=None, desc=""):
        defaults = {"unit": unit, "color": {"mode": "thresholds" if steps else "fixed", "fixedColor": "text"}}
        if steps:
            defaults["thresholds"] = thresholds(*steps)
        if decimals is not None:
            defaults["decimals"] = decimals
        if mappings:
            defaults["mappings"] = mappings
        self.add({"type": "stat", "title": title, "description": desc,
                  "targets": [target(expr, legend, instant=True)],
                  "fieldConfig": {"defaults": defaults, "overrides": []},
                  "options": {"reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False},
                              "colorMode": "background" if steps else "none", "graphMode": "none",
                              "textMode": "value_and_name" if legend else "value", "justifyMode": "center",
                              "orientation": "auto", "wideLayout": True}}, w, h)

    def timeseries(self, title, targets, unit="short", w=12, h=8, steps=None, desc="", stack=False, min0=True, ds=DS):
        """A ratio (percentunit) gets a fixed 0..100% axis."""
        custom = {"drawStyle": "line", "lineWidth": 2, "fillOpacity": 10 if stack else 0, "showPoints": "never",
                  "spanNulls": True, "stacking": {"mode": "normal" if stack else "none", "group": "A"},
                  "thresholdsStyle": {"mode": "line+area" if steps else "off"}}
        defaults = {"unit": unit, "custom": custom, "color": {"mode": "palette-classic"}}
        if min0:
            defaults["min"] = 0
        if unit == "percentunit":
            defaults["max"] = 1
        if steps:
            defaults["thresholds"] = thresholds(*steps)
        self.add({"type": "timeseries", "title": title, "description": desc,
                  "targets": [target(e, l, ref=chr(65 + i), ds=ds) for i, (e, l) in enumerate(targets)],
                  "fieldConfig": {"defaults": defaults, "overrides": []},
                  "options": {"legend": {"displayMode": "list", "placement": "bottom", "showLegend": True},
                              "tooltip": {"mode": "multi", "sort": "desc"}}}, w, h, ds)

    def logs(self, title, expr, w=24, h=16, desc=""):
        self.add({"type": "logs", "title": title, "description": desc,
                  "targets": [target(expr, ds=LOKI) | {"queryType": "range"}],
                  "options": {"showTime": True, "wrapLogMessage": True, "sortOrder": "Descending",
                              "enableLogDetails": True, "dedupStrategy": "none", "prettifyLogMessage": False}}, w, h, LOKI)

    def bars(self, title, expr, legend, unit="short", w=8, h=8, desc=""):
        self.add({"type": "bargauge", "title": title, "description": desc,
                  "targets": [target(expr, legend, instant=True)],
                  "fieldConfig": {"defaults": {"unit": unit, "min": 0, "color": {"mode": "palette-classic"},
                                               "thresholds": thresholds(("text", None))}, "overrides": []},
                  "options": {"orientation": "horizontal", "displayMode": "basic", "showUnfilled": True,
                              "text": {"titleSize": 14, "valueSize": 18}, "minVizHeight": 16, "maxVizHeight": 28,
                              "valueMode": "text", "namePlacement": "left", "sizing": "auto",
                              "reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False}}}, w, h)

    def table(self, title, expr, w=24, h=8, desc=""):
        self.add({"type": "table", "title": title, "description": desc,
                  "targets": [target(expr, instant=True) | {"format": "table"}],
                  "fieldConfig": {"defaults": {"unit": "short"}, "overrides": []},
                  "transformations": [{"id": "organize", "options": {"excludeByName": {"Time": True}}}],
                  "options": {"showHeader": True, "sortBy": [{"displayName": "Value", "desc": True}]}}, w, h)


def dashboard(uid, title, description, builder, links, variables=()):
    return {"uid": uid, "title": title, "description": description, "tags": ["orderflow"],
            "timezone": "browser", "editable": False, "graphTooltip": 1, "refresh": "10s",
            "schemaVersion": 39, "version": 1, "time": {"from": "now-30m", "to": "now"},
            "templating": {"list": list(variables)}, "annotations": {"list": []},
            "links": [{"title": t, "type": "link", "url": f"/d/{u}", "icon": "dashboard"} for t, u in links],
            "panels": builder.panels}


LINKS = [("Business", "orderflow-business"), ("Services", "orderflow-services"), ("Messaging", "orderflow-messaging"),
         ("Logs", "orderflow-logs")]

# ---------- Business ----------
b = Builder()
b.stat("Orders", 'round(sum(increase(orders_created_total[$__range])))', decimals=0,
       desc="Orders created in the selected time range (idempotent replays are not counted). Approximate: increase() extrapolates to the range edges; exact numbers come from the database.")
b.stat("Payment success rate",
       'sum(increase(payments_total{status="succeeded"}[$__range])) / sum(increase(payments_total[$__range]))',
       unit="percentunit", decimals=1, steps=[(BAD, None), (WARN, 0.7), (GOOD, 0.9)])
b.stat("Revenue", 'sum by (currency) (increase(payment_amount_minor_units_total{status="succeeded"}[$__range])) / 100',
       decimals=2, legend="{{currency}}", desc="Successful payments in the selected range, in major units, per currency. Approximate: increase() extrapolates to the range edges; exact numbers come from the database.")
b.stat("Orders shipped", 'round(sum(increase(order_status_changes_total{status="shipped"}[$__range])))', decimals=0,
       desc="Approximate: increase() extrapolates to the range edges; exact numbers come from the database.")
b.timeseries("Orders per minute", [('sum(rate(orders_created_total[1m])) * 60', "orders")], unit="short")
b.timeseries("Payments per minute", [('sum by (status) (rate(payments_total[1m])) * 60', "{{status}}")])
b.bars("Decline reasons", 'round(sum by (reason) (increase(payments_total{status="failed"}[$__range])))', "{{reason}}")
b.bars("Shipments by carrier", 'round(sum by (carrier) (increase(shipments_registered_total[$__range])))', "{{carrier}}")
b.bars("Emails sent", 'round(sum by (kind) (increase(notifications_sent_total[$__range])))', "{{kind}}")
b.timeseries("Order status changes per minute", [('sum by (status) (rate(order_status_changes_total[1m])) * 60', "{{status}}")], w=24)
business = dashboard("orderflow-business", "OrderFlow - Business",
                     "Orders, payments, shipments, and customer emails.", b, LINKS)

# ---------- Services ----------
b = Builder()
up_map = [{"type": "value", "options": {"0": {"text": "DOWN", "color": BAD}, "1": {"text": "UP", "color": GOOD}}}]
b.stat("Service health", f'up{{job=~"{SERVICES}"}}', w=24, h=3, legend="{{job}}", mappings=up_map,
       steps=[(BAD, None), (GOOD, 1)])
b.row("RED: rate, errors, duration")
b.timeseries("Request rate", [('sum by (job) (rate(http_requests_total[1m]))', "{{job}}")], unit="reqps", w=8)
b.timeseries("Error ratio (5xx)", [('(sum by (job) (rate(http_requests_total{status=~"5.."}[5m])) or sum by (job) (rate(http_requests_total[5m])) * 0)'
              ' / sum by (job) (rate(http_requests_total[5m]))', "{{job}}")],
             unit="percentunit", w=8, steps=[("transparent", None), (BAD, 0.05)],
             desc="Share of 5xx responses. The HighErrorRate alert fires above 5%.")
b.timeseries("Latency p95", [('histogram_quantile(0.95, sum by (le, job) (rate(http_request_duration_seconds_bucket[5m])))', "{{job}}")],
             unit="s", w=8, steps=[("transparent", None), (BAD, 1)])
b.timeseries("Gateway latency by route (p50 / p95 / p99)", [
    ('histogram_quantile(0.50, sum by (le, method, route) (rate(http_request_duration_seconds_bucket{job="gateway", route!="unmatched"}[5m])))', "p50 {{method}} {{route}}"),
    ('histogram_quantile(0.95, sum by (le, method, route) (rate(http_request_duration_seconds_bucket{job="gateway", route!="unmatched"}[5m])))', "p95 {{method}} {{route}}"),
    ('histogram_quantile(0.99, sum by (le, method, route) (rate(http_request_duration_seconds_bucket{job="gateway", route!="unmatched"}[5m])))', "p99 {{method}} {{route}}"),
], unit="s", w=12)
b.timeseries("Gateway responses by status", [('sum by (status) (rate(http_requests_total{job="gateway"}[1m]))', "{{status}}")],
             unit="reqps", w=12, desc="429: rate limited, 502/504: upstream unavailable or too slow, 503: database down.")
b.table("Requests by route (selected range)", f'round(sum by (job, method, route, status) (increase(http_requests_total{{job=~"{SERVICES}"}}[$__range]))) > 0')
b.row("Gateway, cache, circuit breakers")
b.timeseries("Rate limiter decisions", [('sum by (policy, decision) (rate(gateway_rate_limit_decisions_total[1m]))', "{{policy}} {{decision}}")],
             unit="reqps", w=8, desc="bypassed: Redis unavailable, requests pass without a limit (fail-open).")
b.timeseries("Idempotency-Key requests", [('sum by (result) (rate(gateway_idempotency_requests_total[1m]))', "{{result}}")],
             unit="reqps", w=8, desc="replayed: the stored response was returned; bypassed: Redis unavailable, the order service deduplicates.")
b.timeseries("Order cache hit ratio", [('sum(rate(order_cache_requests_total{result="hit"}[5m])) / sum(rate(order_cache_requests_total[5m]))', "hit ratio")],
             unit="percentunit", w=8)
breaker_map = [{"type": "value", "options": {"0": {"text": "closed", "color": GOOD}, "1": {"text": "half-open", "color": WARN},
                                             "2": {"text": "OPEN", "color": BAD}}}]
b.stat("Circuit breakers", 'circuit_breaker_state', w=24, h=4, legend="{{job}} · {{name}}", mappings=breaker_map,
       steps=[(GOOD, None), (WARN, 1), (BAD, 2)], desc="Open: calls to the dependency are suspended and fail fast.")
services = dashboard("orderflow-services", "OrderFlow - Services",
                     "RED metrics of every service, the gateway, the cache, and circuit breakers.", b, LINKS)

# ---------- Messaging ----------
b = Builder()
b.stat("Dead-lettered messages", 'sum(rabbitmq_detailed_queue_messages{queue=~".*[.]dlq"}) or vector(0)', decimals=0,
       steps=[(GOOD, None), (BAD, 1)], desc="Messages that failed permanently. Inspect with make dlq-list queue=<service queue>.")
b.stat("Outbox lag", 'max(outbox_oldest_pending_age_seconds)', unit="s", decimals=0,
       steps=[(GOOD, None), (WARN, 10), (BAD, 60)], desc="Age of the oldest unpublished event across services.")
b.stat("Paused consumers", 'sum(messaging_consumer_paused)', decimals=0, steps=[(GOOD, None), (BAD, 1)],
       desc="Consumers wait while their database is unavailable.")
b.stat("Retries per minute", 'sum(rate(messaging_consumed_messages_total{result="retry"}[5m])) * 60 or vector(0)', decimals=1,
       steps=[(GOOD, None), (WARN, 1), (BAD, 10)])
b.row("Consumers")
b.timeseries("Processed messages by result", [('sum by (queue, result) (rate(messaging_consumed_messages_total[1m]))', "{{queue}} {{result}}")],
             unit="ops", w=12, desc="ack: processed, retry: scheduled for a retry, dead_letter: moved to the DLQ.")
b.timeseries("Handler duration p95", [('histogram_quantile(0.95, sum by (le, queue) (rate(messaging_processing_duration_seconds_bucket[5m])))', "{{queue}}")],
             unit="s", w=12)
b.row("Queues (RabbitMQ)")
b.timeseries("Service queues", [('sum by (queue) (rabbitmq_detailed_queue_messages{queue=~".*-service"})', "{{queue}}")],
             unit="short", w=8, desc="Messages waiting to be consumed.")
b.timeseries("Waiting for a retry", [
    ('sum by (service) (label_replace(rabbitmq_detailed_queue_messages{queue=~".*[.]retry[.].*"}, "service", "$1", "queue", "(.*)[.]retry[.].*"))', "{{service}}")],
    unit="short", w=8, desc="Messages in the retry queues (<queue>.retry.<delay>), summed per service.")
b.timeseries("Dead letter queues", [('sum by (queue) (rabbitmq_detailed_queue_messages{queue=~".*[.]dlq"})', "{{queue}}")],
             unit="short", w=8, steps=[("transparent", None), (BAD, 1)])
b.row("Transactional outbox")
b.timeseries("Published events", [('sum by (table) (rate(outbox_published_messages_total[1m]))', "{{table}}")], unit="ops", w=8)
b.timeseries("Pending events", [('outbox_pending_messages', "{{table}}")], unit="short", w=8)
b.timeseries("Oldest pending event", [('outbox_oldest_pending_age_seconds', "{{table}}")], unit="s", w=8,
             steps=[("transparent", None), (BAD, 60)], desc="Grows while the broker is unavailable. The OutboxLagging alert fires above 60s.")
messaging = dashboard("orderflow-messaging", "OrderFlow - Messaging",
                      "Consumers, retries, dead letter queues, RabbitMQ queue depth, and the transactional outbox.", b, LINKS)

# ---------- Logs (Loki) ----------
b = Builder()
APP = '{service=~"$service"}'
b.timeseries("Log lines by level", [(f'sum by (level) (count_over_time({{service=~"$service", level=~".+"}}[1m]))', "{{level}}")],
             w=12, ds=LOKI)
b.timeseries("Errors by service", [(f'sum by (service) (count_over_time({{service=~"$service", level="ERROR"}}[1m]))', "{{service}}")],
             w=12, ds=LOKI, desc="ERROR records per minute. Find the cause in the logs below by request_id or open the trace.")
b.logs("Logs", f'{APP} | request_id=~"$request_id.*"',
       desc="Enter a request ID above to see one order's whole flow across all services. "
            "Expand a record and use the trace_id link to open its trace in Tempo.")
variables = [
    {"type": "custom", "name": "service", "label": "Service", "query": "gateway,order,payment,delivery,notification",
     "multi": True, "includeAll": True, "allValue": "gateway|order|payment|delivery|notification",
     "current": {"selected": True, "text": ["All"], "value": ["$__all"]}},
    {"type": "textbox", "name": "request_id", "label": "Request ID", "query": "",
     "current": {"text": "", "value": ""}},
]
logs = dashboard("orderflow-logs", "OrderFlow - Logs",
                 "Logs of all services in Loki, filterable by request ID, with links to traces in Tempo.", b, LINKS, variables)

for name, d in [("overview", business), ("services", services), ("messaging", messaging), ("logs", logs)]:
    (OUT / f"{name}.json").write_text(json.dumps(d, indent=2, ensure_ascii=False) + "\n")
    print(name, len(d["panels"]), "panels")
