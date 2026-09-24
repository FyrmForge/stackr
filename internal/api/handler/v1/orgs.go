package v1

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
)

type (
	NameIn struct {
		Name string `json:"name"`
	}
	RoleIn struct {
		Role string `json:"role"`
	}
	InviteIn struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	CredentialIn struct {
		Name     string `json:"name"`
		URL      string `json:"url"`
		Username string `json:"username"`
		Password string `json:"password"` // "" keeps the stored one on update
	}
	ConnectorIn struct {
		GitHubOrg string `json:"github_org"`
	}
	ConnectorBegun struct {
		Connector service.Connector `json:"connector"`
		Action    string            `json:"action"`   // the browser POSTs Manifest here
		Manifest  string            `json:"manifest"` // the GitHub App manifest
	}
	KeyOut struct {
		Token string         `json:"token"` // shown once
		Key   service.APIKey `json:"key"`
	}
	PasswordIn struct {
		Current string `json:"current"`
		Next    string `json:"next"`
	}
)

func (in CredentialIn) spec() service.CredentialSpec {
	return service.CredentialSpec{Name: in.Name, URL: in.URL, Username: in.Username, Password: in.Password}
}

func orgID(c echo.Context) string { return scope(c).Org.ID }

// ---- orgs ----

func (h *H) Orgs() Endpoint {
	return Get(func(c echo.Context) ([]service.Org, error) { return list(h.Orch.Orgs(rc(c), who(c))) })
}

func (h *H) CreateOrg() Endpoint {
	return JSON(201, func(c echo.Context, _ None) (service.Org, error) { return h.Orch.CreateOrg(rc(c), who(c)) })
}

func (h *H) GetOrg() Endpoint {
	return Get(func(c echo.Context) (service.Org, error) { return *scope(c).Org, nil })
}

func (h *H) RenameOrg() Endpoint {
	return JSON(200, func(c echo.Context, in NameIn) (service.Org, error) {
		return h.Orch.RenameOrg(rc(c), orgID(c), in.Name)
	})
}

func (h *H) FinishOrg() Endpoint {
	return JSON(200, func(c echo.Context, _ None) (service.Org, error) { return h.Orch.FinishOrg(rc(c), orgID(c)) })
}

func (h *H) DeleteOrg() Endpoint {
	return Done(func(c echo.Context, _ None) error { return h.Orch.DeleteOrg(rc(c), orgID(c)) })
}

// ---- members and invites ----

func (h *H) Members() Endpoint {
	return Get(func(c echo.Context) ([]service.OrgMember, error) { return list(h.Orch.Members(rc(c), orgID(c))) })
}

func (h *H) SetRole() Endpoint {
	return Done(func(c echo.Context, in RoleIn) error {
		return h.Orch.SetRole(rc(c), orgID(c), c.Param("user"), in.Role)
	})
}

func (h *H) RemoveMember() Endpoint {
	return Done(func(c echo.Context, _ None) error { return h.Orch.RemoveMember(rc(c), orgID(c), c.Param("user")) })
}

func (h *H) Invites() Endpoint {
	return Get(func(c echo.Context) ([]service.Invite, error) { return list(h.Orch.Invites(rc(c), orgID(c))) })
}

func (h *H) Invite() Endpoint {
	return JSON(201, func(c echo.Context, in InviteIn) (service.Invite, error) {
		return h.Orch.Invite(rc(c), orgID(c), in.Email, in.Role, who(c))
	})
}

func (h *H) LookupInvite() Endpoint {
	return Get(func(c echo.Context) (service.Invite, error) { return h.Orch.LookupInvite(rc(c), c.Param("token")) })
}

func (h *H) AcceptInvite() Endpoint {
	return Done(func(c echo.Context, _ None) error { return h.Orch.AcceptInvite(rc(c), c.Param("token"), who(c)) })
}

// ---- registry credentials and git connectors ----

func (h *H) Credentials() Endpoint {
	return Get(func(c echo.Context) ([]service.Credential, error) { return list(h.Orch.Credentials(rc(c), orgID(c))) })
}

func (h *H) CreateCredential() Endpoint {
	return JSON(201, func(c echo.Context, in CredentialIn) (service.Credential, error) {
		return h.Orch.CreateCredential(rc(c), orgID(c), in.spec())
	})
}

func (h *H) UpdateCredential() Endpoint {
	return JSON(200, func(c echo.Context, in CredentialIn) (service.Credential, error) {
		return h.Orch.UpdateCredential(rc(c), orgID(c), c.Param("credential"), in.spec())
	})
}

func (h *H) DeleteCredential() Endpoint {
	return Done(func(c echo.Context, _ None) error {
		return h.Orch.DeleteCredential(rc(c), orgID(c), c.Param("credential"))
	})
}

func (h *H) Connectors() Endpoint {
	return Get(func(c echo.Context) ([]service.Connector, error) { return list(h.Orch.Connectors(rc(c), orgID(c))) })
}

func (h *H) BeginConnector() Endpoint {
	return JSON(201, func(c echo.Context, in ConnectorIn) (ConnectorBegun, error) {
		conn, action, manifest, err := h.Orch.BeginConnector(rc(c), orgID(c), in.GitHubOrg)
		return ConnectorBegun{conn, action, manifest}, err
	})
}

func (h *H) RenameConnector() Endpoint {
	return JSON(200, func(c echo.Context, in NameIn) (service.Connector, error) {
		return h.Orch.RenameConnector(rc(c), orgID(c), c.Param("connector"), in.Name)
	})
}

func (h *H) DeleteConnector() Endpoint {
	return Done(func(c echo.Context, _ None) error {
		return h.Orch.DeleteConnector(rc(c), orgID(c), c.Param("connector"))
	})
}

// ---- the caller ----

func (h *H) Me() Endpoint {
	return Get(func(c echo.Context) (service.User, error) { return middleware.Principal(c).User, nil })
}

func (h *H) ChangePassword() Endpoint {
	return Done(func(c echo.Context, in PasswordIn) error {
		return h.Orch.ChangePassword(rc(c), who(c), in.Current, in.Next)
	})
}

func (h *H) Keys() Endpoint {
	return Get(func(c echo.Context) ([]service.APIKey, error) { return list(h.Orch.Keys(rc(c), who(c))) })
}

// MintKey binds the key to the route's org with the caller's live role (B36).
func (h *H) MintKey() Endpoint {
	return JSON(201, func(c echo.Context, in NameIn) (KeyOut, error) {
		tok, k, err := h.Orch.MintKey(rc(c), who(c), orgID(c), in.Name)
		return KeyOut{tok, k}, err
	})
}

// CodeIn is a CLI login exchange.
type CodeIn struct {
	Code string `json:"code"`
}

// CLICode answers the browser that approved a CLI login with the one-time
// code it hands to the CLI's loopback listener.
func (h *H) CLICode() Endpoint {
	return JSON(201, func(c echo.Context, in NameIn) (CodeIn, error) {
		code, err := h.Orch.CLICode(rc(c), who(c), orgID(c), in.Name)
		return CodeIn{code}, err
	})
}

// ExchangeCLICode swaps a code for the key, once.
func (h *H) ExchangeCLICode() Endpoint {
	return JSON(201, func(c echo.Context, in CodeIn) (KeyOut, error) {
		tok, k, err := h.Orch.ExchangeCLICode(rc(c), in.Code)
		return KeyOut{tok, k}, err
	})
}

// MintUnboundKey is the admin's key for no org in particular.
func (h *H) MintUnboundKey() Endpoint {
	return JSON(201, func(c echo.Context, in NameIn) (KeyOut, error) {
		tok, k, err := h.Orch.MintKey(rc(c), who(c), "", in.Name)
		return KeyOut{tok, k}, err
	})
}

func (h *H) RevokeKey() Endpoint {
	return Done(func(c echo.Context, _ None) error { return h.Orch.RevokeKey(rc(c), who(c), c.Param("key")) })
}

func (h *H) Settings() Endpoint {
	return Get(func(c echo.Context) ([]service.Knob, error) { return list(h.Orch.Settings(), nil) })
}
