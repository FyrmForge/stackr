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
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Alerts: the panel posts one JSON body per event to the admin's webhook.
// Every event goes through alert, which never blocks: a bounded queue and one
// delivery goroutine (deliverAlerts). A later notifier subscribes there.
// Each trigger fires once per crossing; the state that says "already sent"
// is the node row (offline/online) or a settings row (disk, certs), so a
// panel restart does not send again.

const (
	alertURLKey    = "alert_webhook_url"
	alertSecretKey = "alert_webhook_secret" // sealed, never in the catalogue
	alertLastKey   = "alert_last_delivery"
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
		slog.Warn("alerts: delivery failed", "event", a.Event, "host", maskURL(u), "err", err)
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
	s.URL = maskURL(u)
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

// maskURL is scheme://host/… for a URL with a path or query.
func maskURL(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return ""
	}
	m := p.Scheme + "://" + p.Host
	if (p.Path != "" && p.Path != "/") || p.RawQuery != "" {
		m += "/…"
	}
	return m
}

// oneLine is s on one line, at most 300 characters.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 300 {
		s = string(r[:300]) + "…"
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
	switch low := diskLow(free, total); {
	case low && !was:
		if err := o.store.Settings.Set(ctx, key, "1"); err != nil {
			return
		}
		pct := uint64(0)
		if total > 0 {
			pct = free * 100 / total
		}
		o.alert(Alert{Event: AlertDiskLow, Severity: SevWarning, Node: n.Name, Subject: n.Name,
			Message: fmt.Sprintf("%s has %s (%d%%) free on its Docker disk.", n.Name, gb(free), pct)})
	case !low && was:
		_ = o.store.Settings.Delete(ctx, key)
	}
}

// controllerDiskAlert is diskAlert for the controller's data dir disk.
func (o *Orchestrator) controllerDiskAlert(ctx context.Context) {
	n, err := o.nodeLeaf().Controller(ctx)
	if err != nil {
		return
	}
	free, total := o.dataDirDisk()
	o.diskAlert(ctx, n, free, total)
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
	switch st := certStage(notAfter, now); st {
	case cur:
	case "":
		_ = o.store.Settings.Delete(ctx, key)
	default:
		if err := o.store.Settings.Set(ctx, key, st); err != nil {
			return
		}
		if cur != "" && st == "14" {
			return // back from 3 to 14: a renewal that is still short, no news
		}
		sev, when := SevWarning, "expires on "+notAfter.UTC().Format(time.DateOnly)
		if st == "3" {
			sev = SevCritical
		}
		if !notAfter.After(now) {
			when = "expired on " + notAfter.UTC().Format(time.DateOnly)
		}
		o.alert(Alert{Event: AlertCertExpiring, Severity: sev, Node: nodeName, Subject: subject,
			Message: "The " + subject + " " + when + "."})
	}
}

// certAlerts checks the cluster root, the controller cert and every
// worker's newest cert. Called from dailyChores.
// ponytail: Let's Encrypt certs live in Caddy's storage, not read here.
func (o *Orchestrator) certAlerts(ctx context.Context) {
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

// backupFailed alerts for a scheduled backup that failed. A skip (node
// offline) or a cancel is not a failure.
func (o *Orchestrator) backupFailed(ctx context.Context, nodeID, subject string, err error) {
	if err == nil || errors.Is(err, context.Canceled) || ctx.Err() != nil || errors.Is(err, jobs.ErrSkipped) {
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
		Message: "Scheduled backup of " + subject + " failed: " + err.Error()})
}
