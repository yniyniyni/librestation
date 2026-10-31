package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const (
	spotifyRedirectHost = "127.0.0.1:8765"
	spotifyRedirectURI  = "http://" + spotifyRedirectHost + "/callback"
	spotifyAuthURL      = "https://accounts.spotify.com/authorize"
	spotifyTokenURL     = "https://accounts.spotify.com/api/token"
	spotifyScopes       = "user-read-playback-state user-modify-playback-state user-read-currently-playing user-library-read user-library-modify"
	spotifyTokenFile    = voiceDir + "/spotify.json"
)

// spotifyPoll is how often the panel is asked whether the link was noticed.
var spotifyPoll = time.Second

type assistantState struct {
	Assistant struct {
		Status        json.RawMessage `json:"status"`
		SpotifyLinked *bool           `json:"spotify_linked"` // null while voiced still checks
	} `json:"assistant"`
}

// checkSpotify wants the token file on the speaker and voiced saying it
// works with it. voiced re-reads the file by mtime; null means it has not
// looked yet, so wait a little.
func checkSpotify(e *env) (bool, string, error) {
	ok, err := e.d.RootOK("test -s " + spotifyTokenFile)
	if err != nil || !ok {
		return false, "", err
	}
	var s assistantState
	for i := 0; i < 15; i++ {
		if i > 0 {
			time.Sleep(spotifyPoll)
		}
		if err := panelState(e, &s); err != nil {
			return false, "панель не отвечает", nil
		}
		if s.Assistant.SpotifyLinked != nil {
			break
		}
	}
	switch {
	case s.Assistant.SpotifyLinked == nil:
		return false, "voiced ещё проверяет привязку", nil
	case !*s.Assistant.SpotifyLinked:
		return false, "токен есть, но Spotify его не принял", nil
	}
	return true, "привязан", nil
}

// checkAssistant is for doctor: the assistant is working when the panel
// reports a status for it. There is nothing to fix here.
func checkAssistant(e *env) (bool, string, error) {
	var s assistantState
	if err := panelState(e, &s); err != nil {
		return false, "панель не отвечает", nil
	}
	if st := strings.TrimSpace(string(s.Assistant.Status)); st == "" || st == "null" {
		return false, "нет статуса (нет ключа Gemini или нет Piper?)", nil
	}
	return true, "", nil
}

func fixSpotify(e *env) error {
	e.u.say("  В приложении на https://developer.spotify.com/dashboard (подойдёт то же, что у Soloist)")
	e.u.say("  добавьте Redirect URI: %s", spotifyRedirectURI)
	clientID := strings.TrimSpace(e.u.ask("  Client ID приложения", ""))
	if clientID == "" {
		return errors.New("Client ID не указан")
	}
	verifier, err := randomString(64)
	if err != nil {
		return err
	}
	state, err := randomString(16)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", spotifyRedirectHost)
	if err != nil {
		return fmt.Errorf("порт 8765 занят: %w", err)
	}
	defer ln.Close()
	link := spotifyAuthURL + "?" + url.Values{
		"client_id":             {clientID},
		"response_type":         {"code"},
		"redirect_uri":          {spotifyRedirectURI},
		"scope":                 {spotifyScopes},
		"state":                 {state},
		"code_challenge_method": {"S256"},
		"code_challenge":        {pkceChallenge(verifier)},
	}.Encode()
	e.u.say("  Открываю браузер. Если не открылся, перейдите по ссылке:\n  %s", link)
	openBrowser(link)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	code, err := awaitCallback(ctx, ln, state)
	if err != nil {
		return err
	}
	refresh, err := exchangeCode(spotifyTokenURL, clientID, code, verifier)
	if err != nil {
		return err
	}
	tok, _ := json.Marshal(map[string]string{"client_id": clientID, "refresh_token": refresh})
	// A file, never a command line.
	return e.d.WriteSecret(spotifyTokenFile, tok)
}

// pkceChallenge is the S256 code challenge of a verifier (RFC 7636).
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b)[:n], nil
}

func openBrowser(link string) {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	exec.Command(name, link).Start()
}

// awaitCallback serves /callback on ln until the browser comes back with a
// code for this state. A request with another state is refused and waited
// past: it is not ours.
func awaitCallback(ctx context.Context, ln net.Listener, state string) (string, error) {
	type result struct {
		code string
		err  error
	}
	res := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var out result
		switch {
		case q.Get("state") != state:
			http.Error(w, "неверный state", http.StatusBadRequest)
			return
		case q.Get("error") != "":
			out.err = fmt.Errorf("Spotify отказал: %s", q.Get("error"))
		case q.Get("code") == "":
			out.err = errors.New("в ответе Spotify нет code")
		default:
			out.code = q.Get("code")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if out.err != nil {
			fmt.Fprint(w, "<p>Не получилось. Вернитесь в терминал.</p>")
		} else {
			fmt.Fprint(w, "<p>Готово, можно закрыть вкладку и вернуться в терминал.</p>")
		}
		select {
		case res <- out:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()
	select {
	case r := <-res:
		return r.code, r.err
	case <-ctx.Done():
		return "", errors.New("не дождался ответа от Spotify (5 минут)")
	}
}

// exchangeCode trades the authorization code for a refresh token, from this
// computer (HTTPS_PROXY applies).
func exchangeCode(tokenURL, clientID, code, verifier string) (string, error) {
	c := http.Client{Timeout: 30 * time.Second}
	r, err := c.PostForm(tokenURL, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {spotifyRedirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	})
	if err != nil {
		return "", fmt.Errorf("обмен кода на токен: %w", err)
	}
	defer r.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	var tok struct {
		RefreshToken string `json:"refresh_token"`
		Error        string `json:"error"`
		Description  string `json:"error_description"`
	}
	json.Unmarshal(body, &tok)
	if r.StatusCode != http.StatusOK || tok.RefreshToken == "" {
		msg := strings.TrimSpace(firstOf(tok.Error+" "+tok.Description, string(body)))
		return "", fmt.Errorf("обмен кода на токен: %s: %s", r.Status, msg)
	}
	return tok.RefreshToken, nil
}
