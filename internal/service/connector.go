package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
	"github.com/FyrmForge/stackr/internal/service/internal/githubapp"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// connector.go holds the server connector verbs added by the server config
// work (the org's connector verbs stay in org.go) and the webhook fan-out of
// a server connector.

// ServerConnector is a connector with no org, as the admin sees it: who it
// is shared with besides. ShareAll wins over OrgIDs. The App private key
// never leaves the leaf.
type ServerConnector struct {
	store.Connector
	OrgIDs []string `json:"org_ids"`
}

func (o *Orchestrator) serverConnector(ctx context.Context, c store.Connector) (ServerConnector, error) {
	ids, err := o.conns.SharedOrgs(ctx, c.ID)
	if ids == nil {
		ids = []string{}
	}
	c.Config = ""
	return ServerConnector{Connector: c, OrgIDs: ids}, err
}

// ServerConnectors lists the server connectors, config stripped.
func (o *Orchestrator) ServerConnectors(ctx context.Context) ([]ServerConnector, error) {
	cs, err := o.conns.ListServer(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ServerConnector, 0, len(cs))
	for _, c := range cs {
		sc, err := o.serverConnector(ctx, c)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, nil
}

// ServerConnectorInstallURL is GitHub's install page for a server
// connector's App; "" while pending.
func (o *Orchestrator) ServerConnectorInstallURL(ctx context.Context, id string) (string, error) {
	return o.conns.ServerInstallURL(ctx, id)
}

// BeginServerConnector makes the pending server connector; the browser POSTs
// manifest to action. userID is the admin who started it; only they can
// complete it.
func (o *Orchestrator) BeginServerConnector(
	ctx context.Context,
	userID, ghOrg string,
) (c Connector, action, manifest string, err error) {
	return o.conns.BeginServer(ctx, userID, ghOrg)
}

// RenameServerConnector renames a server connector (names are unique among
// them: the server file names its connectors).
func (o *Orchestrator) RenameServerConnector(ctx context.Context, id, name string) (ServerConnector, error) {
	c, err := o.conns.RenameServer(ctx, id, name)
	if err != nil {
		return ServerConnector{}, err
	}
	return o.serverConnector(ctx, c)
}

// DeleteServerConnector deletes a server connector, refused while a binding
// (the server file's, an org's or a stack's) names it.
func (o *Orchestrator) DeleteServerConnector(ctx context.Context, id string) error {
	if _, err := o.conns.Server(ctx, id); err != nil {
		return err
	}
	bs, err := o.connectorBindings(ctx, id)
	if err != nil {
		return err
	}
	if len(bs) > 0 {
		return errs.Conflictf("%s binds to this connector; unbind it first.", bs[0].what)
	}
	return o.conns.DeleteServer(ctx, id)
}

// ShareConnector sets who may use a server connector: none (orgIDs empty,
// all false, the default), the named orgs, or all of them. Admin only. A
// share is refused to revoke while a binding in that org names the connector.
func (o *Orchestrator) ShareConnector(
	ctx context.Context,
	id string,
	orgIDs []string,
	all bool,
) (ServerConnector, error) {
	if _, err := o.conns.Server(ctx, id); err != nil {
		return ServerConnector{}, err
	}
	orgIDs = slices.Compact(slices.Sorted(slices.Values(orgIDs)))
	for _, oid := range orgIDs {
		if _, err := o.orgs.Get(ctx, oid); errors.Is(err, errs.ErrNotFound) {
			return ServerConnector{}, errs.Invalidf("org_ids", "no organization %q", oid)
		} else if err != nil {
			return ServerConnector{}, err
		}
	}
	bs, err := o.connectorBindings(ctx, id)
	if err != nil {
		return ServerConnector{}, err
	}
	for _, b := range bs {
		if b.orgID != "" && !all && !slices.Contains(orgIDs, b.orgID) {
			return ServerConnector{}, errs.Conflictf("%s binds to this connector; unbind it before revoking the share.", b.what)
		}
	}
	c, err := o.conns.SetShares(ctx, id, orgIDs, all)
	if err != nil {
		return ServerConnector{}, err
	}
	return o.serverConnector(ctx, c)
}

// binding is one place that names a connector: orgID is the org it belongs
// to, "" for the server's own.
type binding struct{ orgID, what string }

// connectorBindings is everything that names connector id: the server file's
// binding, each org's config file, each stack's config repo.
func (o *Orchestrator) connectorBindings(ctx context.Context, id string) ([]binding, error) {
	var out []binding
	sb, err := o.ServerConfigBinding(ctx)
	if err != nil {
		return nil, err
	}
	if sb.ConnectorID == id {
		out = append(out, binding{what: "The server config file"})
	}
	ogs, err := o.orgs.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	for _, og := range ogs {
		if og.ConfigConnectorID == id {
			out = append(out, binding{og.ID, fmt.Sprintf("The config file of organization %s", og.Slug)})
		}
		sts, err := o.stacks.List(ctx, og.ID)
		if err != nil {
			return nil, err
		}
		for _, st := range sts {
			if st.ConfigConnectorID == id {
				out = append(out, binding{og.ID, fmt.Sprintf("Stack %s/%s", og.Slug, st.Slug)})
			}
		}
	}
	return out, nil
}

// serverWebhook is a push or PR on a server connector's App: a push that
// lands on the server file's binding queues the server plan; then every org
// the connector is shared with gets what its own connector's delivery would
// (its org plan, its stacks that use the repo).
func (o *Orchestrator) serverWebhook(ctx context.Context, c store.Connector, d delivery) error {
	if d.ev.Push != nil {
		if _, err := o.queueServerPlan(ctx, c.ID, d.repo, d.branch, d.defaultBranch); err != nil {
			return err
		}
	}
	ids, err := o.sharedOrgIDs(ctx, c)
	if err != nil {
		return err
	}
	for _, orgID := range ids {
		if err := o.orgWebhook(ctx, orgID, d); err != nil {
			return err
		}
	}
	return nil
}

// sharedOrgIDs is the orgs allowed to read server connector c.
func (o *Orchestrator) sharedOrgIDs(ctx context.Context, c store.Connector) ([]string, error) {
	if !c.ShareAll {
		return o.conns.SharedOrgs(ctx, c.ID)
	}
	ogs, err := o.orgs.ListAll(ctx)
	ids := make([]string, 0, len(ogs))
	for _, og := range ogs {
		ids = append(ids, og.ID)
	}
	return ids, err
}

// delivery is a webhook event reduced to what the fan-out matches on. A PR
// has no branch or default branch; only a push plans a config file.
type delivery struct {
	ev                          githubapp.Event
	repo, branch, defaultBranch string
}

// orgWebhook is what a delivery means for one org: its config file plan
// when a push lands on the org's binding, and a push or PR job per stack of
// the org that uses the repo.
func (o *Orchestrator) orgWebhook(ctx context.Context, orgID string, d delivery) error {
	if p := d.ev.Push; p != nil {
		if err := o.queueOrgPlan(ctx, orgID, d.repo, d.branch, d.defaultBranch); err != nil {
			return err
		}
	}
	sts, err := o.stacks.List(ctx, orgID)
	if err != nil {
		return err
	}
	for _, st := range sts {
		if ok, err := o.usesRepo(ctx, st, d.repo); err != nil || !ok {
			if err != nil {
				return err
			}
			continue
		}
		if p := d.ev.Push; p != nil {
			_, err = o.enqueuePush(ctx, pushJob{
				StackID: st.ID,
				Event: promote.Event{
					Repo:    d.repo,
					Branch:  d.branch,
					Commit:  p.After,
					Message: p.HeadCommit.Message,
					Changed: p.ChangedFiles(),
				},
				DefaultBranch: p.Repository.DefaultBranch,
			})
		} else {
			pr := d.ev.PR
			_, err = o.enqueue(
				ctx,
				kindPR,
				prJob{
					StackID: st.ID,
					Action:  pr.Action,
					Number:  pr.Number,
					Repo:    d.repo,
					Head:    pr.PullRequest.Head.Ref,
					SHA:     pr.PullRequest.Head.SHA,
					Base:    pr.PullRequest.Base.Ref,
				},
				"push:"+st.ID,
				"pr:"+st.ID+":"+strconv.Itoa(pr.Number),
			)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
