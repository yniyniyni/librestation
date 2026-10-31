// Package spotifyapi is a small Spotify Web API client for a PKCE public
// client: it holds a refresh token in a file and never needs a secret.
package spotifyapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrNotLinked means there is no usable Spotify authorization: the token file
// is missing or the refresh token was revoked.
var ErrNotLinked = errors.New("spotify не привязан")

// ErrNoDevice means the speaker is not among the user's Spotify devices.
var ErrNoDevice = errors.New("не вижу колонку в Spotify")

const (
	defaultAPIURL      = "https://api.spotify.com"
	defaultAccountsURL = "https://accounts.spotify.com"
	revokedBackoff     = 60 * time.Second
	expirySlack        = 60 * time.Second
)

// Item is something playable.
type Item struct {
	Kind   string // track, artist, album, playlist
	URI    string
	Name   string
	Artist string // for tracks and albums
}

// Client talks to the Web API as the user whose refresh token is in TokenFile.
type Client struct {
	TokenFile   string // {"client_id":"…","refresh_token":"…"}
	Device      string // Spotify Connect name of the speaker; "" = active or first
	HTTP        *http.Client
	APIURL      string // "" = Spotify
	AccountsURL string // "" = Spotify

	now       func() time.Time // nil = time.Now
	pollDelay time.Duration    // wait between device list retries

	mu         sync.Mutex
	access     string
	expires    time.Time
	revokedAt  time.Time // when invalid_grant was seen; zero = not revoked
	revokedMod time.Time // token file mtime at that moment
}

// New returns a client with default hosts and a 10 s timeout.
func New(tokenFile, device string) *Client {
	return &Client{
		TokenFile: tokenFile,
		Device:    device,
		HTTP:      &http.Client{Timeout: 10 * time.Second},
	}
}

type tokenFile struct {
	ClientID     string `json:"client_id"`
	RefreshToken string `json:"refresh_token"`
}

func (c *Client) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *Client) api() string {
	if c.APIURL != "" {
		return strings.TrimRight(c.APIURL, "/")
	}
	return defaultAPIURL
}

func (c *Client) accounts() string {
	if c.AccountsURL != "" {
		return strings.TrimRight(c.AccountsURL, "/")
	}
	return defaultAccountsURL
}

func (c *Client) httpc() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (c *Client) fileMod() time.Time {
	st, err := os.Stat(c.TokenFile)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

// blocked reports whether calls must fail without a request. A token file
// that changed since the revocation (the user re-ran setup) lifts the block.
// Callers hold c.mu.
func (c *Client) blocked() bool {
	if c.revokedAt.IsZero() {
		return false
	}
	if !c.fileMod().Equal(c.revokedMod) || c.clock().Sub(c.revokedAt) >= revokedBackoff {
		c.revokedAt = time.Time{}
		c.access = ""
		return false
	}
	return true
}

// Linked reports whether Spotify calls can be attempted.
func (c *Client) Linked() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.blocked() {
		return false
	}
	_, err := os.Stat(c.TokenFile)
	return err == nil
}

func (c *Client) readTokenFile() (tokenFile, error) {
	var tf tokenFile
	b, err := os.ReadFile(c.TokenFile)
	if err != nil {
		if os.IsNotExist(err) {
			return tf, ErrNotLinked
		}
		return tf, fmt.Errorf("spotify: токен-файл: %w", err)
	}
	if err := json.Unmarshal(b, &tf); err != nil || tf.RefreshToken == "" || tf.ClientID == "" {
		return tf, ErrNotLinked
	}
	return tf, nil
}

func (c *Client) writeTokenFile(tf tokenFile) error {
	b, err := json.Marshal(tf)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.TokenFile), ".spotify-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), c.TokenFile)
}

// token returns a valid access token, refreshing it when needed.
func (c *Client) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.blocked() {
		return "", ErrNotLinked
	}
	if c.access != "" && c.clock().Before(c.expires.Add(-expirySlack)) {
		return c.access, nil
	}
	tf, err := c.readTokenFile()
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {tf.RefreshToken},
		"client_id":     {tf.ClientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.accounts()+"/api/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpc().Do(req)
	if err != nil {
		return "", fmt.Errorf("spotify: обновление токена: %w", urlErrorOnly(err))
	}
	defer resp.Body.Close()
	var tr struct {
		AccessToken  string `json:"access_token"`
		ExpiresIn    int    `json:"expires_in"`
		RefreshToken string `json:"refresh_token"`
		Error        string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tr)
	if resp.StatusCode != http.StatusOK || tr.AccessToken == "" {
		if tr.Error == "invalid_grant" {
			c.revokedAt = c.clock()
			c.revokedMod = c.fileMod()
			c.access = ""
			return "", ErrNotLinked
		}
		return "", fmt.Errorf("spotify: обновление токена: %s", resp.Status)
	}
	c.access = tr.AccessToken
	c.expires = c.clock().Add(time.Duration(tr.ExpiresIn) * time.Second)
	if tr.RefreshToken != "" && tr.RefreshToken != tf.RefreshToken {
		tf.RefreshToken = tr.RefreshToken
		if err := c.writeTokenFile(tf); err != nil {
			return "", fmt.Errorf("spotify: сохранение токена: %w", err)
		}
	}
	return c.access, nil
}

// urlErrorOnly strips the request URL (which may carry query data) from
// transport errors.
func urlErrorOnly(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

type apiError struct{ status int }

func (e *apiError) Error() string {
	return fmt.Sprintf("spotify: %d %s", e.status, http.StatusText(e.status))
}

// call performs an authorized request; out may be nil. It returns the status
// code; non-2xx statuses are returned as *apiError.
func (c *Client) call(ctx context.Context, method, path string, q url.Values, body any, out any) (int, error) {
	for attempt := 0; ; attempt++ {
		tok, err := c.token(ctx)
		if err != nil {
			return 0, err
		}
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = strings.NewReader(string(b))
		}
		u := c.api() + path
		if len(q) > 0 {
			u += "?" + q.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.httpc().Do(req)
		if err != nil {
			return 0, fmt.Errorf("spotify: %w", urlErrorOnly(err))
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			resp.Body.Close()
			c.mu.Lock()
			c.access = ""
			c.mu.Unlock()
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return resp.StatusCode, &apiError{resp.StatusCode}
		}
		if out != nil && resp.StatusCode != http.StatusNoContent {
			if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
				return resp.StatusCode, fmt.Errorf("spotify: разбор ответа: %w", err)
			}
		}
		return resp.StatusCode, nil
	}
}

type artistRef struct {
	Name string `json:"name"`
}

type searchResp struct {
	Tracks struct {
		Items []struct {
			URI     string      `json:"uri"`
			Name    string      `json:"name"`
			Artists []artistRef `json:"artists"`
		} `json:"items"`
	} `json:"tracks"`
	Artists struct {
		Items []struct {
			URI  string `json:"uri"`
			Name string `json:"name"`
		} `json:"items"`
	} `json:"artists"`
	Albums struct {
		Items []struct {
			URI     string      `json:"uri"`
			Name    string      `json:"name"`
			Artists []artistRef `json:"artists"`
		} `json:"items"`
	} `json:"albums"`
	Playlists struct {
		// Spotify may put nulls in this list.
		Items []*struct {
			URI  string `json:"uri"`
			Name string `json:"name"`
		} `json:"items"`
	} `json:"playlists"`
}

func firstArtist(a []artistRef) string {
	if len(a) == 0 {
		return ""
	}
	return a[0].Name
}

// Search finds one item. kind is track, artist, album, playlist or any.
func (c *Client) Search(ctx context.Context, q, kind string) (Item, error) {
	typ, limit := kind, "1"
	switch kind {
	case "track", "artist", "album", "playlist":
	case "any", "":
		kind, typ, limit = "any", "track,artist,album,playlist", "5"
	default:
		return Item{}, fmt.Errorf("spotify: неизвестный тип %q", kind)
	}
	var r searchResp
	_, err := c.call(ctx, http.MethodGet, "/v1/search", url.Values{"q": {q}, "type": {typ}, "limit": {limit}}, nil, &r)
	if err != nil {
		return Item{}, err
	}
	if kind == "any" {
		for _, a := range r.Artists.Items {
			if strings.EqualFold(strings.TrimSpace(a.Name), strings.TrimSpace(q)) {
				return Item{Kind: "artist", URI: a.URI, Name: a.Name}, nil
			}
		}
		if len(r.Tracks.Items) > 0 {
			t := r.Tracks.Items[0]
			return Item{Kind: "track", URI: t.URI, Name: t.Name, Artist: firstArtist(t.Artists)}, nil
		}
		// Nothing as a track: fall through to the other kinds in order.
		kind = ""
	}
	if kind == "track" || kind == "" {
		if len(r.Tracks.Items) > 0 {
			t := r.Tracks.Items[0]
			return Item{Kind: "track", URI: t.URI, Name: t.Name, Artist: firstArtist(t.Artists)}, nil
		}
	}
	if kind == "artist" || kind == "" {
		if len(r.Artists.Items) > 0 {
			a := r.Artists.Items[0]
			return Item{Kind: "artist", URI: a.URI, Name: a.Name}, nil
		}
	}
	if kind == "album" || kind == "" {
		if len(r.Albums.Items) > 0 {
			a := r.Albums.Items[0]
			return Item{Kind: "album", URI: a.URI, Name: a.Name, Artist: firstArtist(a.Artists)}, nil
		}
	}
	if kind == "playlist" || kind == "" {
		for _, p := range r.Playlists.Items {
			if p != nil {
				return Item{Kind: "playlist", URI: p.URI, Name: p.Name}, nil
			}
		}
	}
	return Item{}, fmt.Errorf("spotify: ничего не найдено по запросу %q", q)
}

type device struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	IsActive bool   `json:"is_active"`
}

// findDevice looks the speaker up, retrying a couple of times because a
// sleeping Connect device can take a moment to show up.
func (c *Client) findDevice(ctx context.Context) (device, error) {
	delay := c.pollDelay
	if delay == 0 {
		delay = time.Second
	}
	for i := 0; i < 3; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return device{}, ctx.Err()
			case <-time.After(delay):
			}
		}
		var r struct {
			Devices []device `json:"devices"`
		}
		if _, err := c.call(ctx, http.MethodGet, "/v1/me/player/devices", nil, nil, &r); err != nil {
			return device{}, err
		}
		for _, d := range r.Devices {
			if d.ID == "" {
				continue
			}
			if c.Device == "" || strings.EqualFold(d.Name, c.Device) {
				if c.Device == "" {
					// Prefer the active device, else the first.
					for _, a := range r.Devices {
						if a.IsActive && a.ID != "" {
							return a, nil
						}
					}
				}
				return d, nil
			}
		}
	}
	return device{}, ErrNoDevice
}

func (c *Client) transfer(ctx context.Context, id string) error {
	_, err := c.call(ctx, http.MethodPut, "/v1/me/player", nil, map[string]any{"device_ids": []string{id}, "play": false}, nil)
	return err
}

// Play starts the item on the speaker, waking it with a transfer if needed.
func (c *Client) Play(ctx context.Context, it Item) error {
	d, err := c.findDevice(ctx)
	if err != nil {
		return err
	}
	body := map[string]any{}
	if it.Kind == "track" {
		body["uris"] = []string{it.URI}
	} else {
		body["context_uri"] = it.URI
	}
	play := func() (int, error) {
		return c.call(ctx, http.MethodPut, "/v1/me/player/play", url.Values{"device_id": {d.ID}}, body, nil)
	}
	if !d.IsActive {
		if err := c.transfer(ctx, d.ID); err != nil {
			return err
		}
	}
	status, err := play()
	if status == http.StatusNotFound {
		if err := c.transfer(ctx, d.ID); err != nil {
			return err
		}
		_, err = play()
	}
	return err
}

// Current returns what is playing now; false when nothing is.
func (c *Client) Current(ctx context.Context) (Item, bool, error) {
	var r struct {
		Item *struct {
			Type    string      `json:"type"`
			URI     string      `json:"uri"`
			Name    string      `json:"name"`
			Artists []artistRef `json:"artists"`
		} `json:"item"`
	}
	status, err := c.call(ctx, http.MethodGet, "/v1/me/player/currently-playing", nil, nil, &r)
	if err != nil {
		return Item{}, false, err
	}
	if status == http.StatusNoContent || r.Item == nil {
		return Item{}, false, nil
	}
	kind := r.Item.Type
	if kind == "" {
		kind = "track"
	}
	return Item{Kind: kind, URI: r.Item.URI, Name: r.Item.Name, Artist: firstArtist(r.Item.Artists)}, true, nil
}

// SetLiked saves a track to (or removes it from) the user's library.
func (c *Client) SetLiked(ctx context.Context, uri string, liked bool) error {
	id, ok := strings.CutPrefix(uri, "spotify:track:")
	if !ok || id == "" {
		return fmt.Errorf("spotify: не трек: %q", uri)
	}
	method := http.MethodPut
	if !liked {
		method = http.MethodDelete
	}
	// TODO(verify): Spotify may have replaced /v1/me/tracks?ids= with a
	// generic /v1/me/library endpoint; check the current docs on a device.
	_, err := c.call(ctx, method, "/v1/me/tracks", url.Values{"ids": {id}}, nil, nil)
	return err
}
