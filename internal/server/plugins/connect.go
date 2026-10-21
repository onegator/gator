package plugins

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/oauth2"

	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/plugin"
)

// ErrNotConnectable means the server cannot run a plugin's connection: the plugin declares no
// OAuth flow, or this server has no OAuth client or public address for it.
var ErrNotConnectable = errors.New("plugin cannot be connected")

// OAuthClient is the server's registration with a provider.
type OAuthClient struct{ ID, Secret string }

// OAuthClientFromEnv reads GATOR_<PROVIDER>_CLIENT_ID and GATOR_<PROVIDER>_CLIENT_SECRET.
func OAuthClientFromEnv(provider string) (OAuthClient, bool) {
	p := "GATOR_" + strings.ToUpper(strings.ReplaceAll(provider, "-", "_"))
	c := OAuthClient{ID: os.Getenv(p + "_CLIENT_ID"), Secret: os.Getenv(p + "_CLIENT_SECRET")}
	return c, c.ID != "" && c.Secret != ""
}

// connectStateTTL is how long a person has to finish signing in at the provider.
const connectStateTTL = 15 * time.Minute

// refreshKey holds a connection's refresh token next to the access token, out of the plugin's
// reach (see serverOnly).
func refreshKey(secret string) string { return "_refresh_" + secret }

// ConnectView is a plugin's connection as the API shows it.
type ConnectView struct {
	Title     string
	Available bool // the server can run the flow
	Connected bool // a token is stored
}

func (h *Host) connectView(name string, m plugin.Manifest, secretsSet map[string]bool) *ConnectView {
	c := m.Connect
	if c == nil {
		return nil
	}
	_, ok := h.cfg.OAuthClient(c.ProviderName(name))
	title := c.Title
	if title == "" {
		title = name
	}
	return &ConnectView{Title: title, Available: ok && h.cfg.PublicURL != "", Connected: secretsSet[c.Secret]}
}

func (h *Host) redirectURL() string { return h.cfg.PublicURL + "/connect/callback" }

func (h *Host) oauthConfig(name string, c *plugin.ConnectSpec) (*oauth2.Config, error) {
	if c == nil {
		return nil, fmt.Errorf("%w: %s has no connection to make", ErrNotConnectable, name)
	}
	provider := c.ProviderName(name)
	client, ok := h.cfg.OAuthClient(provider)
	if !ok {
		env := "GATOR_" + strings.ToUpper(strings.ReplaceAll(provider, "-", "_"))
		return nil, fmt.Errorf("%w: this server has no %s OAuth app; set %s_CLIENT_ID and %s_CLIENT_SECRET", ErrNotConnectable, provider, env, env)
	}
	if h.cfg.PublicURL == "" {
		return nil, fmt.Errorf("%w: set GATOR_PUBLIC_URL so the provider can send the browser back", ErrNotConnectable)
	}
	// AuthStyleInParams: GitHub and Linear both take the client in the form, and probing for
	// the style would spend a single-use code on the wrong guess.
	return &oauth2.Config{ClientID: client.ID, ClientSecret: client.Secret, RedirectURL: h.redirectURL(),
		Endpoint: oauth2.Endpoint{AuthURL: c.AuthorizeURL, TokenURL: c.TokenURL, AuthStyle: oauth2.AuthStyleInParams}}, nil
}

// ReturnAllowed says whether the browser may be sent on to uri after connecting: Gator.app's own
// scheme, or a loopback address. Anything else would let a link send a person somewhere else
// with Gator's word that something went well.
func ReturnAllowed(uri string) bool {
	if uri == "" {
		return true
	}
	u, err := url.Parse(uri)
	if err != nil || u.User != nil || u.Fragment != "" {
		return false
	}
	switch u.Scheme {
	case "gator":
		return true
	case "http":
		ip := net.ParseIP(u.Hostname())
		return u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
	}
	return false
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func hashState(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// Connect starts a connection: it remembers who asked, for which project and plugin, and
// answers the provider's authorization URL for the person's browser.
func (h *Host) Connect(ctx context.Context, projectID, userID pgtype.UUID, name, returnTo string) (string, error) {
	if !ReturnAllowed(returnTo) {
		return "", fmt.Errorf("%w: return_to must be a gator:// or loopback address", ErrInvalid)
	}
	p, err := h.ensureInstalled(ctx, name)
	if err != nil {
		return "", err
	}
	var m plugin.Manifest
	_ = json.Unmarshal(p.Manifest, &m)
	cfg, err := h.oauthConfig(name, m.Connect)
	if err != nil {
		return "", err
	}
	q := db.New(h.pool)
	_ = q.DeleteExpiredPluginConnectStates(ctx)
	state, verifier := randomToken(), oauth2.GenerateVerifier()
	if err := q.InsertPluginConnectState(ctx, db.InsertPluginConnectStateParams{
		Hash: hashState(state), ProjectID: projectID, PluginName: name, UserID: userID, Verifier: verifier,
		ReturnTo: returnTo, ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(connectStateTTL), Valid: true},
	}); err != nil {
		return "", err
	}
	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(verifier)}
	if len(m.Connect.Scopes) > 0 {
		sep := m.Connect.ScopeSeparator
		if sep == "" {
			sep = " "
		}
		opts = append(opts, oauth2.SetAuthURLParam("scope", strings.Join(m.Connect.Scopes, sep)))
	}
	for k, v := range m.Connect.Params {
		opts = append(opts, oauth2.SetAuthURLParam(k, v))
	}
	return cfg.AuthCodeURL(state, opts...), nil
}

// ConnectHandler serves GET /connect/callback, where the provider sends the browser back. It
// needs no Gator token: the single-use state is the proof, and it names the project.
func (h *Host) ConnectHandler() http.Handler {
	r := chi.NewRouter()
	r.Get("/callback", h.connectCallback)
	return r
}

var connectPage = template.Must(template.New("connect").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>Gator</title>
<style>body{font:16px -apple-system,system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem;color:#222}
@media (prefers-color-scheme:dark){body{background:#1c1c1e;color:#eee}}</style></head>
<body><h1>{{.Title}}</h1><p>{{.Detail}}</p></body></html>`))

func connectAnswer(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = connectPage.Execute(w, map[string]string{"Title": title, "Detail": detail})
}

func (h *Host) connectCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	qv := r.URL.Query()
	st, err := db.New(h.pool).TakePluginConnectState(ctx, hashState(qv.Get("state")))
	if errors.Is(err, pgx.ErrNoRows) {
		connectAnswer(w, http.StatusBadRequest, "This link has expired",
			"Start again from the project's settings in Gator.")
		return
	}
	if err != nil {
		connectAnswer(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	finish := func(status int, failure string) {
		if st.ReturnTo != "" {
			u, _ := url.Parse(st.ReturnTo)
			v := u.Query()
			v.Set("plugin", st.PluginName)
			v.Set("project", uuidString(st.ProjectID))
			if failure != "" {
				v.Set("error", failure)
			} else {
				v.Set("status", "connected")
			}
			u.RawQuery = v.Encode()
			http.Redirect(w, r, u.String(), http.StatusFound)
			return
		}
		if failure != "" {
			connectAnswer(w, status, "Not connected", failure)
			return
		}
		connectAnswer(w, http.StatusOK, "Connected", "You can close this window and go back to Gator.")
	}
	if e := qv.Get("error"); e != "" {
		msg := qv.Get("error_description")
		if msg == "" {
			msg = e
		}
		finish(http.StatusBadRequest, "The provider said: "+msg)
		return
	}
	if err := h.completeConnect(ctx, st, qv.Get("code")); err != nil {
		h.log.Warn("plugin connection failed", "plugin", st.PluginName, "project", uuidString(st.ProjectID), "err", err)
		finish(http.StatusBadGateway, "Gator could not finish connecting: "+err.Error())
		return
	}
	finish(http.StatusOK, "")
}

// completeConnect trades the code for a token and stores it as the project's secret. The
// plugin, if it is on, restarts with it, and a tracker imports what it can now read.
func (h *Host) completeConnect(ctx context.Context, st db.PluginConnectState, code string) error {
	if code == "" {
		return errors.New("the provider sent no code")
	}
	q := db.New(h.pool)
	p, err := q.GetPluginByName(ctx, st.PluginName)
	if err != nil {
		return err
	}
	var m plugin.Manifest
	_ = json.Unmarshal(p.Manifest, &m)
	cfg, err := h.oauthConfig(st.PluginName, m.Connect)
	if err != nil {
		return err
	}
	tok, err := cfg.Exchange(context.WithValue(ctx, oauth2.HTTPClient, h.cfg.HTTP), code, oauth2.VerifierOption(st.Verifier))
	if err != nil {
		return describeOAuthErr(err)
	}
	if tok.AccessToken == "" {
		return errors.New("the provider answered without a token")
	}
	return h.storeToken(ctx, st.ProjectID, p, m.Connect, tok, true)
}

// storeToken writes a token into the project's secrets, leaving every other secret alone.
func (h *Host) storeToken(ctx context.Context, projectID pgtype.UUID, p db.Plugin, c *plugin.ConnectSpec, tok *oauth2.Token, first bool) error {
	q := db.New(h.pool)
	sec := map[string]string{}
	enabled := false
	cur, err := q.GetProjectPlugin(ctx, db.GetProjectPluginParams{ProjectID: projectID, Name: p.Name})
	switch {
	case err == nil:
		if sec, err = h.decryptSecrets(cur.Secrets); err != nil {
			return err
		}
		enabled = cur.Enabled
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	sec[c.Secret] = tok.AccessToken
	delete(sec, refreshKey(c.Secret))
	if tok.RefreshToken != "" {
		sec[refreshKey(c.Secret)] = tok.RefreshToken
	}
	if c.WebhookSecret != "" && sec[c.WebhookSecret] == "" {
		sec[c.WebhookSecret] = randomToken()
	}
	if h.keys == nil {
		return ErrNoKeyring
	}
	b, _ := json.Marshal(sec)
	enc, err := h.keys.Encrypt(b)
	if err != nil {
		return err
	}
	// Only a token that can be renewed gets an expiry: one that cannot will simply stop
	// working, and the plugin's failures say so where a person will see it.
	var exp pgtype.Timestamptz
	if tok.RefreshToken != "" && !tok.Expiry.IsZero() {
		exp = pgtype.Timestamptz{Time: tok.Expiry, Valid: true}
	}
	if _, err := q.SetProjectPluginSecrets(ctx, db.SetProjectPluginSecretsParams{ProjectID: projectID, PluginID: p.ID,
		Secrets: enc, TokenExpiresAt: exp}); err != nil {
		return err
	}
	if first {
		_ = emit(ctx, q, "plugin.connected", "project", projectID, map[string]any{"plugin": p.Name})
	}
	if err := h.Sync(ctx); err != nil {
		return err
	}
	if first && enabled {
		h.importLater(projectID, p.Name)
	}
	return nil
}

// RefreshTokens renews connected tokens that expire within the next quarter of an hour.
func (h *Host) RefreshTokens(ctx context.Context) {
	q := db.New(h.pool)
	rows, err := q.ListExpiringPluginTokens(ctx, pgtype.Timestamptz{Time: time.Now().Add(15 * time.Minute), Valid: true})
	if err != nil {
		return
	}
	for _, r := range rows {
		if err := h.refreshOne(ctx, r); err != nil && ctx.Err() == nil {
			h.log.Warn("refreshing a plugin's token", "plugin", r.PluginName, "project", uuidString(r.ProjectID), "err", err)
		}
	}
}

func (h *Host) refreshOne(ctx context.Context, r db.ListExpiringPluginTokensRow) error {
	q := db.New(h.pool)
	var m plugin.Manifest
	_ = json.Unmarshal(r.Manifest, &m)
	sec, err := h.decryptSecrets(r.Secrets)
	if err != nil {
		return err
	}
	stop := func(why error) error {
		// Nothing left to refresh with: stop trying. The plugin's own failures will say the
		// token stopped working, and connecting again starts over.
		_ = q.ClearPluginTokenExpiry(ctx, r.ID)
		return why
	}
	if m.Connect == nil || sec[refreshKey(m.Connect.Secret)] == "" {
		return stop(errors.New("no refresh token"))
	}
	cfg, err := h.oauthConfig(r.PluginName, m.Connect)
	if err != nil {
		return err
	}
	old := &oauth2.Token{RefreshToken: sec[refreshKey(m.Connect.Secret)], Expiry: time.Unix(1, 0)}
	tok, err := cfg.TokenSource(context.WithValue(ctx, oauth2.HTTPClient, h.cfg.HTTP), old).Token()
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.Response != nil && re.Response.StatusCode < 500 {
			return stop(describeOAuthErr(err))
		}
		return describeOAuthErr(err)
	}
	if tok.RefreshToken == "" {
		tok.RefreshToken = old.RefreshToken
	}
	p, err := q.GetPluginByName(ctx, r.PluginName)
	if err != nil {
		return err
	}
	return h.storeToken(ctx, r.ProjectID, p, m.Connect, tok, false)
}

// describeOAuthErr keeps the provider's own words and drops the response dump.
func describeOAuthErr(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		if re.ErrorDescription != "" {
			return errors.New(re.ErrorDescription)
		}
		if re.ErrorCode != "" {
			return errors.New(re.ErrorCode)
		}
		if re.Response != nil {
			return fmt.Errorf("the provider answered %s", re.Response.Status)
		}
	}
	return err
}
