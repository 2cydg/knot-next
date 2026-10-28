package coreclient

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// Write DTOs deliberately have no presence, recent-use, runtime or secret fields.
type ServerWrite struct {
	ID             string   `json:"id"`
	Alias          string   `json:"alias"`
	Host           string   `json:"host"`
	Port           int      `json:"port"`
	User           string   `json:"user"`
	AuthMethod     string   `json:"auth_method"`
	KeyID          string   `json:"key_id"`
	KnownHostsPath string   `json:"known_hosts_path"`
	ProxyID        string   `json:"proxy_id"`
	JumpHostIDs    []string `json:"jump_host_ids"`
	Tags           []string `json:"tags"`
}

func ServerInput(s Server) ServerWrite {
	return ServerWrite{s.ID, s.Alias, s.Host, s.Port, s.User, s.AuthMethod, s.KeyID, s.KnownHostsPath, s.ProxyID, s.JumpHostIDs, s.Tags}
}

type ProxyWrite struct {
	ID       string `json:"id"`
	Alias    string `json:"alias"`
	Type     string `json:"type"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
}

func ProxyInput(p Proxy) ProxyWrite {
	return ProxyWrite{p.ID, p.Alias, p.Type, p.Host, p.Port, p.Username}
}

type KeyWrite struct {
	ID    string `json:"id"`
	Alias string `json:"alias"`
}
type ProviderWrite struct {
	ID        string `json:"id"`
	Alias     string `json:"alias"`
	Type      string `json:"type"`
	URL       string `json:"url"`
	Username  string `json:"username"`
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	Region    string `json:"region"`
	Endpoint  string `json:"endpoint"`
	PathStyle bool   `json:"path_style"`
}

func ProviderInput(p SyncProvider) ProviderWrite {
	return ProviderWrite{p.ID, p.Alias, p.Type, p.URL, p.Username, p.Bucket, p.Key, p.Region, p.Endpoint, p.PathStyle}
}

type PasswordWrite struct {
	Password string `json:"password"`
}
type KeyMaterialWrite struct {
	SourcePath string `json:"source_path,omitempty"`
	PrivateKey string `json:"private_key,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
}
type S3CredentialsWrite struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
}
type Resource string

const (
	Servers   Resource = "servers"
	Keys      Resource = "keys"
	Proxies   Resource = "proxies"
	Providers Resource = "sync-providers"
)

func (r Resource) valid() bool { return r == Servers || r == Keys || r == Proxies || r == Providers }
func resourcePath(r Resource, id string) string {
	return "/v1/config/" + string(r) + "/" + url.PathEscape(id)
}
func (c *Client) CheckInstance(ctx context.Context, instance string) error {
	r, err := c.Runtime(ctx)
	if err != nil {
		return err
	}
	if instance == "" || r.InstanceID != instance {
		return Failure("INSTANCE_CHANGED")
	}
	return nil
}
func (c *Client) WriteServer(ctx context.Context, create bool, v ServerWrite) (Server, error) {
	var out Server
	err := c.writeResource(ctx, Servers, v.ID, create, v, &out)
	return out, err
}
func (c *Client) WriteProxy(ctx context.Context, create bool, v ProxyWrite) (Proxy, error) {
	var out Proxy
	err := c.writeResource(ctx, Proxies, v.ID, create, v, &out)
	return out, err
}
func (c *Client) WriteKey(ctx context.Context, create bool, v KeyWrite) (Key, error) {
	var out Key
	err := c.writeResource(ctx, Keys, v.ID, create, v, &out)
	return out, err
}
func (c *Client) WriteProvider(ctx context.Context, create bool, v ProviderWrite) (SyncProvider, error) {
	var out SyncProvider
	err := c.writeResource(ctx, Providers, v.ID, create, v, &out)
	return out, err
}
func (c *Client) writeResource(ctx context.Context, kind Resource, id string, create bool, v, out any) error {
	method, path := http.MethodPut, resourcePath(kind, id)
	if create {
		method, path = http.MethodPost, "/v1/config/"+string(kind)
	}
	return c.request(ctx, method, path, v, out)
}
func (c *Client) Server(ctx context.Context, id string) (Server, error) {
	var v Server
	err := c.get(ctx, resourcePath(Servers, id), &v)
	return v, err
}
func (c *Client) Proxy(ctx context.Context, id string) (Proxy, error) {
	var v Proxy
	err := c.get(ctx, resourcePath(Proxies, id), &v)
	return v, err
}
func (c *Client) Key(ctx context.Context, id string) (Key, error) {
	var v Key
	err := c.get(ctx, resourcePath(Keys, id), &v)
	return v, err
}
func (c *Client) Provider(ctx context.Context, id string) (SyncProvider, error) {
	var v SyncProvider
	err := c.get(ctx, resourcePath(Providers, id), &v)
	return v, err
}
func (c *Client) DeleteResource(ctx context.Context, kind Resource, id string) error {
	if !kind.valid() || id == "" {
		return Failure("PROTOCOL_ERROR")
	}
	return c.request(ctx, http.MethodDelete, resourcePath(kind, id), nil, nil)
}
func (c *Client) Password(ctx context.Context, kind Resource, id string, value string, clear bool) error {
	if kind != Servers && kind != Proxies && kind != Providers {
		return Failure("PROTOCOL_ERROR")
	}
	method := http.MethodPut
	if clear {
		method = http.MethodDelete
	}
	return c.request(ctx, method, "/v1/secrets/"+string(kind)+"/"+url.PathEscape(id)+"/password", PasswordWrite{value}, nil)
}
func (c *Client) KeyMaterial(ctx context.Context, id string, value KeyMaterialWrite, clear bool) error {
	method := http.MethodPut
	if clear {
		method = http.MethodDelete
	}
	return c.request(ctx, method, "/v1/secrets/keys/"+url.PathEscape(id)+"/private", value, nil)
}
func (c *Client) S3Credentials(ctx context.Context, id string, value S3CredentialsWrite, clear bool) error {
	method := http.MethodPut
	if clear {
		method = http.MethodDelete
	}
	return c.request(ctx, method, "/v1/secrets/sync-providers/"+url.PathEscape(id)+"/s3-credentials", value, nil)
}
func (c *Client) SyncPassword(ctx context.Context, value string, clear bool) error {
	method := http.MethodPut
	if clear {
		method = http.MethodDelete
	}
	return c.request(ctx, method, "/v1/secrets/sync/password", PasswordWrite{value}, nil)
}
func (c *Client) DefaultProvider(ctx context.Context, id string, clear bool) error {
	method := http.MethodPut
	if clear {
		method = http.MethodDelete
	}
	return c.request(ctx, method, resourcePath(Providers, id)+"/default", nil, nil)
}

type ProxyDraft struct {
	Type     string  `json:"type"`
	Host     string  `json:"host"`
	Port     int     `json:"port"`
	Username string  `json:"username"`
	Password *string `json:"password,omitempty"`
}
type ProxyTestRequest struct {
	ProxyID   string      `json:"proxy_id,omitempty"`
	Draft     *ProxyDraft `json:"draft,omitempty"`
	Target    string      `json:"target,omitempty"`
	TimeoutMS int         `json:"timeout_ms,omitempty"`
}
type ProxyTestResult struct {
	Passed      bool             `json:"passed"`
	Scope       string           `json:"scope"`
	Stages      []ProxyTestStage `json:"stages"`
	FailureCode string           `json:"failure_code"`
}
type ProxyTestStage struct {
	Name       string `json:"name"`
	Passed     bool   `json:"passed"`
	DurationMS int64  `json:"duration_ms"`
}

func (c *Client) TestProxy(ctx context.Context, v ProxyTestRequest) (ProxyTestResult, error) {
	var out ProxyTestResult
	err := c.request(ctx, http.MethodPost, "/v1/proxy-tests", v, &out)
	return out, err
}

type ForwardDefinition struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Bind    string `json:"bind"`
	Target  string `json:"target"`
	Enabled bool   `json:"enabled"`
}
type ForwardRequest struct {
	ServerID     string `json:"server_id"`
	ConnectionID string `json:"connection_id,omitempty"`
	Type         string `json:"type"`
	Bind         string `json:"bind"`
	Target       string `json:"target"`
}
type Forward struct {
	ID           string    `json:"id"`
	ServerID     string    `json:"server_id"`
	ConnectionID string    `json:"connection_id"`
	Type         string    `json:"type"`
	Bind         string    `json:"bind"`
	Target       string    `json:"target"`
	BoundAddress string    `json:"bound_address"`
	Origin       string    `json:"origin"`
	DefinitionID string    `json:"definition_id"`
	State        string    `json:"state"`
	FailureCode  string    `json:"failure_code"`
	CreatedAt    time.Time `json:"created_at"`
}
type Connection struct {
	ID        string    `json:"id"`
	ServerID  string    `json:"server_id"`
	CreatedAt time.Time `json:"created_at"`
}

func (c *Client) ForwardDefinitions(ctx context.Context, id string) ([]ForwardDefinition, error) {
	var v []ForwardDefinition
	err := c.get(ctx, resourcePath(Servers, id)+"/forwards", &v)
	return v, err
}
func (c *Client) PutForwardDefinitions(ctx context.Context, id string, v []ForwardDefinition) error {
	return c.request(ctx, http.MethodPut, resourcePath(Servers, id)+"/forwards", struct {
		Forwards []ForwardDefinition `json:"forwards"`
	}{v}, nil)
}
func (c *Client) Forwards(ctx context.Context, serverID string) ([]Forward, error) {
	var v []Forward
	err := c.get(ctx, "/v1/forwards?server_id="+url.QueryEscape(serverID), &v)
	return v, err
}
func (c *Client) Connections(ctx context.Context, serverID string) ([]Connection, error) {
	var v []Connection
	err := c.get(ctx, "/v1/connections?server_id="+url.QueryEscape(serverID), &v)
	return v, err
}
func (c *Client) CreateForward(ctx context.Context, v ForwardRequest) (Forward, error) {
	var out Forward
	err := c.request(ctx, http.MethodPost, "/v1/forwards", v, &out)
	return out, err
}
func (c *Client) ControlForward(ctx context.Context, id, op string) (Forward, error) {
	var out Forward
	if op != "start" && op != "stop" && op != "delete" {
		return out, Failure("PROTOCOL_ERROR")
	}
	method, path := http.MethodPost, "/v1/forwards/"+url.PathEscape(id)+"/control"
	var body any = struct {
		Op string `json:"op"`
	}{op}
	if op == "delete" {
		method, path, body = http.MethodDelete, "/v1/forwards/"+url.PathEscape(id), nil
	}
	err := c.request(ctx, method, path, body, &out)
	return out, err
}
