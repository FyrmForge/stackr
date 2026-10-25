package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Alerts: the panel posts one JSON body per event to the admin's webhook.
// Every event goes through alert, which never blocks: a bounded queue and one
// delivery goroutine (deliverAlerts). A later notifier subscribes there.
// Each trigger fires once per crossing; the state that says "already sent"
// is the node row (offline/online) or a settings row (disk, certs, backups), so a
// panel restart does not send again.

const (
	alertURLKey    = "alert_webhook_url"
	alertSecretKey = "alert_webhook_secret" // sealed, never in the catalogue
	alertLastKey   = "alert_last_delivery"
	heartbeatKey   = "heartbeat_url"
	// AlertSignatureHeader carries sha256=<hex HMAC of the body> when a secret is set.
	AlertSignatureHeader = "X-Stackr-Signature"
)

// Alert events.
const (
	AlertNodeOffline  = "node.offline"
	AlertNodeOnline   = "node.online"
	AlertBackupFailed = "backup.failed"
	AlertCertExpiring = "cert.expiring"
	AlertDiskLow      = "disk.low"
	AlertClockSkew    = "clock.skew"
	AlertObserveDisk  = "observe.disk"
	AlertCertFailing  = "cert.failing"
	AlertCertOK       = "cert.ok"
	AlertCrashloop    = "container.crashloop"
	AlertCrashOK      = "container.recovered"
	AlertMemoryLow    = "memory.low"
	AlertMemoryOK     = "memory.ok"
	AlertTest         = "test"
)

// Alert severities.
const (
	SevInfo     = "info"
	SevWarning  = "warning"
	SevCritical = "critical"
)

// Vars for tests.
var (
	alertTries   = 4
	alertBackoff = 2 * time.Second // doubles each retry
	alertClient  = &http.Client{Timeout: 10 * time.Second}
)

// Alert is the webhook body.
type Alert struct {
	Event    string `json:"event"`
	Severity string `json:"severity"`
	Node     string `json:"node"`
	Subject  string `json:"subject"`
	Message  string `json:"message"`
	At       string `json:"at"` // RFC3339
	PanelURL string `json:"panel_url"`
}

// AlertDelivery is the last post's outcome.
type AlertDelivery struct {
	OK     bool      `json:"ok"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

// AlertSettings is the Alerts tab and GET /admin/alerts. URL is masked to
// scheme://host/…: a webhook's path is often its token.
type AlertSettings struct {
	URL       string         `json:"url"`
	SecretSet bool           `json:"secret_set"`
	Heartbeat string         `json:"heartbeat"` // masked like URL; "" = off
	Last      *AlertDelivery `json:"last_delivery,omitempty"`
}

// alert queues a for delivery; a full queue drops it (logged).
func (o *Orchestrator) alert(a Alert) {
	if a.At == "" {
		a.At = time.Now().UTC().Format(time.RFC3339)
	}
	a.Message = oneLine(a.Message)
	select {
	case o.alerts <- a:
	default:
		slog.Warn("alerts: queue full, dropped", "event", a.Event, "subject", a.Subject)
	}
}

// deliverAlerts posts queued alerts until the panel stops.
func (o *Orchestrator) deliverAlerts(ctx context.Context) {
	defer o.alertsDone.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case a := <-o.alerts:
			_ = o.sendAlert(ctx, a, alertTries)
		}
	}
}

// startAlerts starts the delivery goroutine; Close stops and waits for it.
func (o *Orchestrator) startAlerts() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-o.stopNodes
		cancel()
	}()
	o.alertsDone.Add(1)
	go o.deliverAlerts(ctx)
}

var errAlertsOff = errs.Conflictf("No alert webhook is set.")

// sendAlert posts a with up to tries attempts, retrying network errors, 5xx
// and 429 with backoff, and records the outcome. No URL is a no-op.
func (o *Orchestrator) sendAlert(ctx context.Context, a Alert, tries int) error {
	u, err := o.settings.Get(ctx, alertURLKey)
	if err != nil {
		return err
	}
	if u == "" {
		return errAlertsOff
	}
	secret, err := o.store.Settings.GetSealed(ctx, alertSecretKey)
	if err != nil {
		return err
	}
	a.PanelURL = o.panelURL()
	body, err := json.Marshal(a)
	if err != nil {
		return err
	}
	for i := range tries {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(alertBackoff << (i - 1)):
			}
		}
		var retry bool
		if retry, err = postAlert(ctx, u, secret, body); err == nil || !retry {
			break
		}
	}
	if ctx.Err() != nil {
		return ctx.Err() // stopping: nothing recorded
	}
	o.recordDelivery(ctx, err)
	if err != nil {
		slog.Warn("alerts: delivery failed", "event", a.Event, "host", settings.MaskURL(u), "err", err)
	}
	return err
}

// alertSig is the signature header's value for body.
func alertSig(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// postAlert makes one attempt; retry says whether another may succeed.
func postAlert(ctx context.Context, u, secret string, body []byte) (retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return false, errors.New("bad URL")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "stackr-alerts")
	if secret != "" {
		req.Header.Set(AlertSignatureHeader, alertSig(secret, body))
	}
	resp, err := alertClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL may hold a token: never in the reason
		}
		return true, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return false, nil
	}
	return resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests,
		fmt.Errorf("HTTP %d", resp.StatusCode)
}

func (o *Orchestrator) recordDelivery(ctx context.Context, err error) {
	d := AlertDelivery{OK: err == nil, At: time.Now().UTC()}
	if err != nil {
		d.Reason = oneLine(err.Error())
	}
	b, _ := json.Marshal(d)
	if werr := o.store.Settings.Set(ctx, alertLastKey, string(b)); werr != nil {
		slog.Warn("alerts: last delivery not kept", "err", werr)
	}
}

// AlertSettings (admin) is the webhook as the Alerts tab shows it.
func (o *Orchestrator) AlertSettings(ctx context.Context) (AlertSettings, error) {
	var s AlertSettings
	u, err := o.settings.Get(ctx, alertURLKey)
	if err != nil {
		return s, err
	}
	s.URL = settings.MaskURL(u)
	hb, err := o.settings.Get(ctx, heartbeatKey)
	if err != nil {
		return s, err
	}
	s.Heartbeat = settings.MaskURL(hb)
	sec, _, err := o.store.Settings.Get(ctx, alertSecretKey)
	if err != nil {
		return s, err
	}
	s.SecretSet = sec != ""
	if v, ok, err := o.store.Settings.Get(ctx, alertLastKey); err != nil {
		return s, err
	} else if ok {
		var d AlertDelivery
		if json.Unmarshal([]byte(v), &d) == nil {
			s.Last = &d
		}
	}
	return s, nil
}

// SetAlerts (admin) sets the webhook URL; a nil secret keeps the stored one,
// "" clears it. An empty URL keeps the stored one (the form leaves it blank).
func (o *Orchestrator) SetAlerts(ctx context.Context, rawURL string, secret *string) error {
	if rawURL = strings.TrimSpace(rawURL); rawURL != "" {
		if err := o.SetSetting(ctx, alertURLKey, rawURL); err != nil {
			return err
		}
	} else if cur, err := o.settings.Get(ctx, alertURLKey); err != nil || cur == "" {
		if err == nil {
			err = errs.Invalidf("url", "Give the webhook URL.")
		}
		return err
	}
	if secret == nil {
		return nil
	}
	if *secret == "" {
		return o.store.Settings.Delete(ctx, alertSecretKey)
	}
	return o.store.Settings.SetSealed(ctx, alertSecretKey, *secret)
}

// SetHeartbeat (admin) sets the dead-man's-switch URL; "" turns it off.
func (o *Orchestrator) SetHeartbeat(ctx context.Context, rawURL string) error {
	rawURL = strings.TrimSpace(rawURL)
	if u, err := url.Parse(rawURL); rawURL != "" && (err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "") {
		return errs.Invalidf("heartbeat", "Give an http or https URL.")
	}
	if strings.HasSuffix(rawURL, "/…") {
		return errs.Invalidf("heartbeat", "That is the masked URL. Paste the full URL.")
	}
	return o.SetSetting(ctx, heartbeatKey, rawURL)
}

// ClearAlerts (admin) turns alerts off: URL, secret and last delivery go.
func (o *Orchestrator) ClearAlerts(ctx context.Context) error {
	if err := o.SetSetting(ctx, alertURLKey, ""); err != nil {
		return err
	}
	if err := o.store.Settings.Delete(ctx, alertSecretKey); err != nil {
		return err
	}
	return o.store.Settings.Delete(ctx, alertLastKey)
}

// TestAlert (admin) posts a test event now, once, and says how it went.
func (o *Orchestrator) TestAlert(ctx context.Context) error {
	err := o.sendAlert(ctx, Alert{
		Event:    AlertTest,
		Severity: SevInfo,
		Subject:  "test",
		Message:  "Test alert from stackr.",
		At:       time.Now().UTC().Format(time.RFC3339),
	}, 1)
	if err == nil || errors.Is(err, errAlertsOff) {
		return err
	}
	if _, ok := errs.IsConflict(err); ok {
		return err
	}
	return errs.Conflictf("Test delivery failed: %s.", oneLine(err.Error()))
}

// oneLine is s on one line, at most 300 characters. A longer s loses its
// middle: an error's tail is usually the cause.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 300 {
		s = string(r[:150]) + "…" + string(r[len(r)-149:])
	}
	return s
}

// ---- triggers ----

// nodeEvent is the alert for a worker's status change: offline only from
// online, back online only from offline (a first join says nothing).
func nodeEvent(name, prev, next string) (Alert, bool) {
	switch {
	case prev == node.StatusOnline && next == node.StatusOffline:
		return Alert{Event: AlertNodeOffline, Severity: SevCritical, Node: name, Subject: name,
			Message: name + " is offline."}, true
	case prev == node.StatusOffline && next == node.StatusOnline:
		return Alert{Event: AlertNodeOnline, Severity: SevInfo, Node: name, Subject: name,
			Message: name + " is back online."}, true
	}
	return Alert{}, false
}

// diskLow is the deploy gate's line: under 1 GB or under 10% free.
func diskLow(free, total uint64) bool {
	return free < minFreeDisk || (total > 0 && free*100 < total*lowDiskPct)
}

func diskAlertKey(nodeID string) string { return "alert.disk." + nodeID }

// diskAlert fires once when a node's Docker disk crosses diskLow and resets
// when it recovers. A disk the node did not report changes nothing.
func (o *Orchestrator) diskAlert(ctx context.Context, n store.Node, free, total uint64) {
	if free == 0 && total == 0 {
		return
	}
	key := diskAlertKey(n.ID)
	_, was, err := o.store.Settings.Get(ctx, key)
	if err != nil {
		return
	}
	low := diskLow(free, total)
	if !low {
		o.clearProblem(ctx, key)
		if was {
			_ = o.store.Settings.Delete(ctx, key)
		}
		return
	}
	pct := uint64(0)
	if total > 0 {
		pct = free * 100 / total
	}
	msg := fmt.Sprintf("%s has %s (%d%%) free on its Docker disk.", n.Name, gb(free), pct)
	o.openProblem(ctx, key, SevWarning, msg)
	if !was && o.store.Settings.Set(ctx, key, "1") == nil {
		o.alert(Alert{Event: AlertDiskLow, Severity: SevWarning, Node: n.Name, Subject: n.Name, Message: msg})
	}
}

// maxSkew is how far a worker's clock may be from the controller's before
// it gets a warning. Nothing is refused over it.
const maxSkew = 30 * time.Second

// skewed: a clock skew past maxSkew either way.
func skewed(d time.Duration) bool { return d > maxSkew || d < -maxSkew }

// skewWords reads a skew as "2m ahead" or "45s behind".
func skewWords(d time.Duration) string {
	dir := "ahead"
	if d < 0 {
		d, dir = -d, "behind"
	}
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh %s", d.Round(time.Hour)/time.Hour, dir)
	case d >= time.Minute:
		return fmt.Sprintf("%dm %s", d.Round(time.Minute)/time.Minute, dir)
	}
	return fmt.Sprintf("%ds %s", d.Round(time.Second)/time.Second, dir)
}

func clockAlertKey(nodeID string) string { return "alert.clock." + nodeID }

// clockAlert fires once when a worker's clock goes past maxSkew and resets
// when it is back. An unknown skew (an older worker) changes nothing.
func (o *Orchestrator) clockAlert(ctx context.Context, n store.Node, skew time.Duration, known bool) {
	if !known {
		return
	}
	key := clockAlertKey(n.ID)
	_, was, err := o.store.Settings.Get(ctx, key)
	if err != nil {
		return
	}
	if !skewed(skew) {
		o.clearProblem(ctx, key)
		if was {
			_ = o.store.Settings.Delete(ctx, key)
		}
		return
	}
	msg := fmt.Sprintf("The clock of %s is %s of the panel. Check time sync on the node.", n.Name, skewWords(skew))
	o.openProblem(ctx, key, SevWarning, msg)
	if !was && o.store.Settings.Set(ctx, key, "1") == nil {
		o.alert(Alert{Event: AlertClockSkew, Severity: SevWarning, Node: n.Name, Subject: n.Name, Message: msg})
	}
}

// controllerDiskAlert is diskAlert for the controller, read as the deploy gate reads it.
func (o *Orchestrator) controllerDiskAlert(ctx context.Context) {
	defer o.alertTick(ctx, time.Now())
	n, err := o.nodeLeaf().Controller(ctx)
	if err != nil {
		return
	}
	free, total := o.diskOf(ctx, n.ID)
	o.diskAlert(ctx, n, free, total)
	o.observeDiskAlert(ctx, n, free)
}

const observeAlertKey = "alert.observe.disk"

// observeDiskAlert fires once when observability is on and the controller's
// free disk, less what the logs may still grow to, falls under twice
// observe_min_free (VictoriaMetrics stops writing at the floor), and resets
// when it recovers. Turning observability off clears it.
func (o *Orchestrator) observeDiskAlert(ctx context.Context, n store.Node, free uint64) {
	if !o.ObserveOn() {
		_ = o.store.Settings.Delete(ctx, observeAlertKey)
		o.clearProblem(ctx, observeAlertKey)
		return
	}
	if free == 0 {
		return
	}
	k, err := o.observeKnobs(ctx)
	if err != nil {
		return
	}
	_, was, err := o.store.Settings.Get(ctx, observeAlertKey)
	if err != nil {
		return
	}
	var room uint64 // the logs' growth to their cap
	if k.logs {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		used, _ := o.victoriaLogs().DataSize(cctx)
		cancel()
		room = k.logsMax - min(used, k.logsMax)
	}
	if free >= 2*k.minFree+room {
		o.clearProblem(ctx, observeAlertKey)
		if was {
			_ = o.store.Settings.Delete(ctx, observeAlertKey)
		}
		return
	}
	msg := fmt.Sprintf("%s has %s free. Metrics history stops at %s.", n.Name, gb(free), gb(k.minFree))
	o.openProblem(ctx, observeAlertKey, SevWarning, msg)
	if !was && o.store.Settings.Set(ctx, observeAlertKey, "1") == nil {
		o.alert(Alert{Event: AlertObserveDisk, Severity: SevWarning, Node: n.Name, Subject: n.Name, Message: msg})
	}
}

// certStage is "3" under 3 days left (or expired), "14" under 14, "" else.
func certStage(notAfter, now time.Time) string {
	switch left := notAfter.Sub(now); {
	case left < 3*24*time.Hour:
		return "3"
	case left < certWarnDays*24*time.Hour:
		return "14"
	}
	return ""
}

// certAlert fires when a cert enters the 14 or 3 day stage, once each; a
// renewal drops the stage and the next crossing fires again.
func (o *Orchestrator) certAlert(ctx context.Context, id, nodeName, subject string, notAfter, now time.Time) {
	key := "alert.cert." + id
	cur, _, err := o.store.Settings.Get(ctx, key)
	if err != nil {
		return
	}
	st := certStage(notAfter, now)
	if st == "" {
		o.clearProblem(ctx, key)
		if cur != "" {
			_ = o.store.Settings.Delete(ctx, key)
		}
		return
	}
	sev, when := SevWarning, "expires on "+notAfter.UTC().Format(time.DateOnly)
	if st == "3" {
		sev = SevCritical
	}
	if !notAfter.After(now) {
		when = "expired on " + notAfter.UTC().Format(time.DateOnly)
	}
	msg := "The " + subject + " " + when + "."
	o.openProblem(ctx, key, sev, msg)
	if st == cur || o.store.Settings.Set(ctx, key, st) != nil {
		return
	}
	if cur != "" && st == "14" {
		return // back from 3 to 14: a renewal that is still short, no news
	}
	o.alert(Alert{Event: AlertCertExpiring, Severity: sev, Node: nodeName, Subject: subject, Message: msg})
}

// certAlerts checks the cluster root, the controller cert and every
// worker's newest cert. Called from dailyChores.
// ponytail: Let's Encrypt certs live in Caddy's storage, not read here.
func (o *Orchestrator) certAlerts(ctx context.Context) {
	o.hostCertAlerts(ctx)
	c := o.cluster
	if c == nil {
		return
	}
	now := time.Now()
	ctl, _ := o.nodeLeaf().Controller(ctx)
	c.mu.Lock()
	root := c.ca.Cert.NotAfter
	own := c.own
	c.mu.Unlock()
	o.certAlert(ctx, "root", ctl.Name, "cluster root certificate", root, now)
	leaf := own.Leaf
	if leaf == nil && len(own.Certificate) > 0 {
		leaf, _ = x509.ParseCertificate(own.Certificate[0])
	}
	if leaf != nil {
		o.certAlert(ctx, "controller", ctl.Name, "controller certificate", leaf.NotAfter, now)
	}
	ns, err := node.New(o.store.Nodes).List(ctx)
	if err != nil {
		return
	}
	for _, n := range ns {
		if n.Role != node.Worker {
			continue
		}
		cs, err := o.store.NodeCerts.ListByNode(ctx, n.ID)
		if err != nil {
			continue
		}
		if t := newestCert(cs); !t.IsZero() {
			o.certAlert(ctx, "node."+n.ID, n.Name, "node certificate of "+n.Name, t, now)
		}
	}
}

// backupRemind is how long a schedule fails before backup.failed fires again.
const backupRemind = 24 * time.Hour

// backupResult takes a scheduled backup's outcome; key names the schedule.
// The first failure alerts, then a reminder each backupRemind it keeps
// failing; a success resets. A skip (node offline) or a cancel is neither.
// The row alert.backup.<key> holds when the last alert went.
func (o *Orchestrator) backupResult(ctx context.Context, key, nodeID, subject string, err error) {
	if errors.Is(err, context.Canceled) || ctx.Err() != nil || errors.Is(err, jobs.ErrSkipped) {
		return
	}
	key = "alert.backup." + key
	last, was, gerr := o.store.Settings.Get(ctx, key)
	if gerr != nil {
		return
	}
	if err == nil {
		if was {
			_ = o.store.Settings.Delete(ctx, key)
		}
		o.clearProblem(ctx, key)
		return
	}
	what := "Backup of " + subject
	if key == "alert.backup.panel" {
		what = "Panel backup"
	}
	o.openProblem(ctx, key, SevCritical, what+" failing: "+err.Error())
	verb := "failed"
	if was {
		t, perr := time.Parse(time.RFC3339, last)
		if perr == nil && time.Since(t) < backupRemind {
			return
		}
		verb = "still fails"
	}
	if o.store.Settings.Set(ctx, key, time.Now().UTC().Format(time.RFC3339)) != nil {
		return
	}
	name := "controller"
	if nodeID == "" {
		nodeID, _ = o.controllerID(ctx)
	}
	if n, nerr := o.store.Nodes.Get(ctx, nodeID); nerr == nil {
		name = n.Name
	}
	o.alert(Alert{Event: AlertBackupFailed, Severity: SevCritical, Node: name, Subject: subject,
		Message: "Scheduled backup of " + subject + " " + verb + ": " + err.Error()})
}

// ---- the watch tick: heartbeat, crash loops, memory ----

const (
	heartbeatEvery = time.Minute
	crashWindow    = 10 * time.Minute
	crashRestarts  = 3 // restarts within crashWindow that make a loop
	memLowFor      = 5 * time.Minute
	memLowPct      = 10 // MemAvailable share of total under which memory is low
)

// alertState is what the tick keeps between runs. The "already sent" marks
// live in settings rows; this is only the windows that have no row to be.
type alertState struct {
	lastBeat, lastPoll time.Time
	restarts           map[string][]restartSample // container id -> samples inside crashWindow
	memLowSince        map[string]time.Time       // node id -> when MemAvailable first read low
}

type restartSample struct {
	at    time.Time
	count int
}

// alertWatch is the one alertState. The tick runs on one goroutine.
func (o *Orchestrator) alertWatch() *alertState { return o.alertMem }

// alertTick runs every 30s with the controller's disk check.
func (o *Orchestrator) alertTick(ctx context.Context, now time.Time) {
	o.heartbeat(ctx, now)
	o.memoryAlerts(ctx, now)
	if w := o.alertWatch(); now.Sub(w.lastPoll) >= time.Minute {
		w.lastPoll = now
		o.containerAlerts(ctx, now)
	}
}

// heartbeat GETs the dead-man's-switch URL once a minute while one is set. A
// failed GET is logged: the switch alerts from its own side when pings stop.
func (o *Orchestrator) heartbeat(ctx context.Context, now time.Time) {
	u, err := o.settings.Get(ctx, heartbeatKey)
	w := o.alertWatch()
	if err != nil || u == "" || now.Sub(w.lastBeat) < heartbeatEvery {
		return
	}
	w.lastBeat = now
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", "stackr-heartbeat")
	resp, err := alertClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL is a token: never in the log
		}
		slog.Warn("heartbeat: ping failed", "err", err)
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// memLow: MemAvailable under memLowPct of the total. Unreported is not low.
func memLow(free, total uint64) bool { return free > 0 && total > 0 && free*100 < total*memLowPct }

// memoryAlerts fires memory.low when a node's MemAvailable stays low for
// memLowFor, and memory.ok when it recovers. Facts are the mesh's last
// report; a node with none changes nothing.
func (o *Orchestrator) memoryAlerts(ctx context.Context, now time.Time) {
	if o.mesh == nil {
		return
	}
	ns, err := o.nodeLeaf().List(ctx)
	if err != nil {
		return
	}
	for _, n := range ns {
		if f, ok := o.mesh.factsOf(n.ID); ok {
			o.memAlert(ctx, n, f.MemFree, f.MemBytes, now)
		}
	}
}

func (o *Orchestrator) memAlert(ctx context.Context, n store.Node, free, total uint64, now time.Time) {
	w, key := o.alertWatch(), "alert.mem."+n.ID
	if free == 0 || total == 0 {
		return
	}
	_, was, err := o.store.Settings.Get(ctx, key)
	if err != nil {
		return
	}
	if !memLow(free, total) {
		delete(w.memLowSince, n.ID)
		o.clearProblem(ctx, key)
		if was {
			_ = o.store.Settings.Delete(ctx, key)
			o.alert(Alert{Event: AlertMemoryOK, Severity: SevInfo, Node: n.Name, Subject: n.Name,
				Message: n.Name + " has memory to spare again."})
		}
		return
	}
	since, ok := w.memLowSince[n.ID]
	if !ok {
		w.memLowSince[n.ID], since = now, now
	}
	if !was && now.Sub(since) < memLowFor {
		return // low, not for long enough yet
	}
	msg := fmt.Sprintf("%s has %s (%d%%) of memory available.", n.Name, gb(free), free*100/total)
	o.openProblem(ctx, key, SevWarning, msg)
	if !was && o.store.Settings.Set(ctx, key, "1") == nil {
		o.alert(Alert{Event: AlertMemoryLow, Severity: SevWarning, Node: n.Name, Subject: n.Name, Message: msg})
	}
}

// containerAlerts reads every tile container's restart count and OOM flag on
// every node and hands them to crashAlert and oomAlert.
// ponytail: one inspect per tile container a minute; a Docker events stream if that ever shows.
func (o *Orchestrator) containerAlerts(ctx context.Context, now time.Time) {
	looping := map[string]string{} // tile id -> node name
	seen := map[string]bool{}
	partial := false // a node or a container that did not answer
	err := o.eachNode(ctx, func(ctx context.Context, n store.Node) error {
		cs, err := o.docker.List(ctx, map[string]string{tile.LabelRole: "replica"})
		for _, c := range cs {
			tid := c.Labels[tile.LabelTile]
			if tid == "" {
				continue
			}
			d, ierr := o.docker.Inspect(ctx, c.ID)
			if ierr != nil {
				partial = true
				continue
			}
			seen[tid] = true
			if o.crashSample(c.ID, d.RestartCount, now) {
				looping[tid] = n.Name
			}
			o.oomAlert(ctx, tid, n, d)
		}
		return err
	})
	for tid := range seen {
		o.crashAlert(ctx, tid, looping[tid])
	}
	if err != nil || partial {
		return
	}
	// a looping tile that was stopped or deleted is never seen again: clear it
	o.sweep(ctx, "crash.", func(id string) bool { return seen[id] })
}

// crashSample records a container's restart count and says whether it went
// up by crashRestarts within crashWindow.
func (o *Orchestrator) crashSample(id string, count int, now time.Time) bool {
	w := o.alertWatch()
	ss := slices.Clone(w.restarts[id])
	ss = append(ss, restartSample{now, count})
	for len(ss) > 1 && now.Sub(ss[1].at) > crashWindow { // keep one sample at or before the window's start
		ss = ss[1:]
	}
	w.restarts[id] = ss
	return count-ss[0].count >= crashRestarts
}

// crashAlert fires once when a tile starts crash looping (node is the node
// name, "" = not looping) and again as recovered when it stops.
func (o *Orchestrator) crashAlert(ctx context.Context, tileID, node string) {
	key := "alert.crash." + tileID
	_, was, err := o.store.Settings.Get(ctx, key)
	if err != nil {
		return
	}
	looping := node != ""
	if !looping {
		o.clearProblem(ctx, key)
	}
	switch {
	case !looping && !was, looping && was && o.hasProblem(ctx, key):
		return // nothing new: the lookups below are only for news
	}
	app, env := tileID, ""
	if t, err := o.store.Tiles.Get(ctx, tileID); err == nil {
		app = t.Name
		if e, err := o.store.Environments.Get(ctx, t.EnvironmentID); err == nil {
			env = e.Name
		}
	}
	if looping {
		msg := fmt.Sprintf("%s in %s keeps restarting on %s: %d restarts in %d minutes.", app, env, node, crashRestarts, crashWindow/time.Minute)
		o.openProblem(ctx, key, SevCritical, msg)
		if !was && o.store.Settings.Set(ctx, key, "1") == nil {
			o.alert(Alert{Event: AlertCrashloop, Severity: SevCritical, Node: node, Subject: app, Message: msg})
		}
		return
	}
	_ = o.store.Settings.Delete(ctx, key)
	o.alert(Alert{Event: AlertCrashOK, Severity: SevInfo, Subject: app,
		Message: fmt.Sprintf("%s in %s stopped restarting.", app, env)})
}

// oomAlert fires memory.low once per OOM kill (the container's restart count
// at the kill is the mark); a kill is an event, so it has no recovery.
func (o *Orchestrator) oomAlert(ctx context.Context, tileID string, n store.Node, d docker.Detail) {
	key := "alert.oom." + d.ID
	last, was, err := o.store.Settings.Get(ctx, key)
	if err != nil {
		return
	}
	if !d.OOMKilled {
		if was {
			_ = o.store.Settings.Delete(ctx, key)
		}
		return
	}
	if mark := strconv.Itoa(d.RestartCount); last == mark || o.store.Settings.Set(ctx, key, mark) != nil {
		return
	}
	app := tileID
	if t, err := o.store.Tiles.Get(ctx, tileID); err == nil {
		app = t.Name
	}
	o.alert(Alert{Event: AlertMemoryLow, Severity: SevCritical, Node: n.Name, Subject: app,
		Message: app + " on " + n.Name + " was killed for running out of memory."})
}
