package main

import (
	crand "crypto/rand"
	"crypto/tls"
	"encoding/base32"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/crypto/blake2b"
	"golang.org/x/crypto/ssh/terminal"
)

var (
	dirFlag   = flag.String("dir", "/var/lib/humbox", "Data directory")
	httpFlag  = flag.String("http", ":8080", "Serve HTTP at given address")
	httpsFlag = flag.String("https", "", "Serve HTTPS at given address")
	certFlag  = flag.String("cert", "", "Use the provided TLS certificate")
	ckeyFlag  = flag.String("cert-key", "", "Use the provided TLS certificate key")
	acmeFlag  = flag.String("acme", "", "Auto-request TLS certs as the given account email")
	tokenFlag = flag.Bool("token", false, "Generate hash and salt for an account token")
)

var httpServer = &http.Server{
	ReadTimeout:  10 * time.Second,
	WriteTimeout: 30 * time.Second,
}

var httpClient = &http.Client{
	Timeout: 10 * time.Second,
}

var manager *Manager

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func dirPath(parts ...string) string {
	return filepath.Join(append([]string{*dirFlag}, parts...)...)
}

func prepareHumboxToken() error {
	oldState, err := terminal.MakeRaw(0)
	if err != nil {
		panic(err)
	}
	defer terminal.Restore(0, oldState)

	term := terminal.NewTerminal(os.Stdin, "")

	term.SetPrompt("Account name: ")
	account, err := term.ReadLine()
	if err != nil || account == "" {
		return err
	}
	token, err := term.ReadPassword("Token (empty for random): ")
	if err != nil {
		return err
	}
	var randomToken bool
	if token == "" {
		var random [20]byte
		if _, err := crand.Read(random[:]); err != nil {
			return fmt.Errorf("cannot obtain random token with %d bytes: %v", len(random), err)
		}
		token = strings.ToLower(base32.HexEncoding.EncodeToString(random[:]))
		randomToken = true
	} else {
		again, err := term.ReadPassword("Please repeat to confirm: ")
		if err != nil {
			return err
		}
		if token != again {
			return fmt.Errorf("the two entered tokens do not match")
		}
	}

	var salt [8]byte
	if _, err := crand.Read(salt[:]); err != nil {
		return fmt.Errorf("cannot obtain random salt with %d bytes: %v", len(salt), err)
	}
	hash := blake2b.Sum256(append([]byte(token), salt[:]...))

	terminal.Restore(0, oldState)

	fmt.Printf("\n\n")
	fmt.Printf("===> Setup for humbox\n\nInsert or append this in setup.yaml:\n\n")
	fmt.Printf("accounts:\n  - name: %s\n    hash: v1:%s\n\n\n", account, strings.ToLower(base32.HexEncoding.EncodeToString(append(hash[:], salt[:]...))))
	fmt.Printf("===> Your API token\n\n")
	if randomToken {
		fmt.Printf("%s\n\n", token)
	}
	fmt.Printf("Store it safely and access the API using the account name\nas your username and your token as the password.\n\n")
	return nil
}

func run() error {
	flag.Parse()

	if *tokenFlag {
		return prepareHumboxToken()
	}

	rand.Seed(time.Now().UnixNano())

	http.HandleFunc("/", handler)

	if *httpFlag == "" && *httpsFlag == "" {
		return fmt.Errorf("must provide -http and/or -https")
	}
	if *acmeFlag != "" && *httpsFlag == "" {
		return fmt.Errorf("cannot use -acme without -https")
	}
	if *acmeFlag != "" && (*certFlag != "" || *ckeyFlag != "") {
		return fmt.Errorf("cannot provide -acme with -key or -cert")
	}
	if *acmeFlag == "" && (*httpsFlag != "" || *certFlag != "" || *ckeyFlag != "") && (*httpsFlag == "" || *certFlag == "" || *ckeyFlag == "") {
		return fmt.Errorf("-https -cert and -key must be used together")
	}

	var err error
	manager, err = NewManager(*dirFlag)
	if err != nil {
		return err
	}

	ch := make(chan error, 2)

	if *httpFlag != "" && (*httpsFlag == "" || *acmeFlag == "") {
		server := *httpServer
		server.Addr = *httpFlag
		go func() {
			ch <- server.ListenAndServe()
		}()
	}
	if *httpsFlag != "" {
		server := *httpServer
		server.Addr = *httpsFlag
		if *acmeFlag != "" {
			if err := os.MkdirAll(filepath.Join(*dirFlag, "acme"), 0700); err != nil {
				return fmt.Errorf("cannot write into %s directory: %v", *dirFlag, err)
			}
			m := autocert.Manager{
				ForceRSA:    true,
				Prompt:      autocert.AcceptTOS,
				Cache:       autocert.DirCache(dirPath("acme")),
				RenewBefore: 24 * 30 * time.Hour,
				HostPolicy:  autocert.HostWhitelist(manager.Hostname()),
				Email:       *acmeFlag,
			}
			server.TLSConfig = &tls.Config{
				GetCertificate: m.GetCertificate,
			}
			go func() {
				ch <- http.ListenAndServe(":80", m.HTTPHandler(nil))
			}()
		}
		go func() {
			ch <- server.ListenAndServeTLS(*certFlag, *ckeyFlag)
		}()

	}
	listenAddr := *httpsFlag
	if listenAddr == "" {
		listenAddr = *httpFlag
	}
	log.Printf("Listening at %s", listenAddr)
	return <-ch
}

func handler(resp http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/health-check" {
		resp.Write([]byte("ok"))
		return
	}

	req.ParseForm()

	if req.URL.Path == "/" {
		log.Printf("Request by %s to %s %s", req.RemoteAddr, req.Method, req.URL)
		resp.Header().Set("Location", "http://github.com/snapcore/spread")
		resp.WriteHeader(http.StatusTemporaryRedirect)
		return
	}

	accountName, token, ok := req.BasicAuth()
	if !ok {
		log.Printf("Request by %s to %s %s", req.RemoteAddr, req.Method, req.URL)
		sendError(resp, errAuth, "missing account and token authentication information")
		return
	}
	log.Printf("Request by %s@%s to %s %s", accountName, req.RemoteAddr, req.Method, req.URL)
	account, err := manager.Account(accountName)
	if err == nil {
		err = account.Authenticate(token)
	}
	if err != nil {
		sendError(resp, err)
		return
	}

	if req.URL.Path == "/v1/servers" {
		serversHandler(resp, req, account)
		return
	}
	if m := serverPattern.FindStringSubmatch(req.URL.Path); m != nil {
		serverHandler(resp, req, account, m[1])
		return
	}

	sendNotFound(resp, req)
}

func decodeRequest(resp http.ResponseWriter, req *http.Request, result interface{}) bool {
	body, err := ioutil.ReadAll(req.Body)
	if err != nil {
		sendError(resp, requestError{fmt.Errorf("cannot read request body: %v", err)})
		return false
	}
	if err = json.Unmarshal(body, &result); err != nil {
		sendError(resp, requestError{fmt.Errorf("cannot decode request body: %v", err)})
		return false
	}
	return true
}

func serversHandler(resp http.ResponseWriter, req *http.Request, account *Account) {
	switch req.Method {
	case "GET":
		servers, err := manager.Servers(account)
		if err != nil {
			sendError(resp, errInternal, "cannot list servers: %s", err)
			return
		}
		if servers == nil {
			servers = []*Server{}
		}
		result := map[string]interface{}{"servers": servers}
		sendJSON(resp, http.StatusOK, result)

	case "POST":
		var payload struct {
			Image    string
			Password string
			Custom   map[string]interface{} `json:"custom"`
		}
		if !decodeRequest(resp, req, &payload) {
			return
		}

		if payload.Image == "" {
			sendError(resp, errRequest, "missing image parameter")
			return
		}
		if payload.Password == "" {
			sendError(resp, errRequest, "missing password parameter")
			return
		}

		image, err := manager.Image(payload.Image)
		if err != nil {
			sendError(resp, err)
			return
		}
		server, err := manager.Allocate(account, image, payload.Password, payload.Custom)
		if err != nil {
			sendError(resp, errInternal, "cannot allocate server: %s", err)
			return
		}
		if err = manager.Start(server); err != nil {
			//_ = manager.Deallocate(server)
			sendError(resp, errInternal, "cannot start server %s: %s", server.Name, err)
			return
		}
		result := map[string]interface{}{"ok": true, "server": server}
		sendJSON(resp, http.StatusOK, result)

	default:
		sendBadMethod(resp, req)
	}
}

var serverPattern = regexp.MustCompile(`/v1/servers/([a-z]{3}[0-9]{6}-[0-9]{3})`)

func serverHandler(resp http.ResponseWriter, req *http.Request, account *Account, serverName string) {
	server, err := manager.Server(serverName)
	if err != nil {
		log.Printf("Account %q attempted to handle non-existent server %s.", account.Name, server.Name)
		sendError(resp, err)
		return
	}

	if !account.Admin && server.Account != account.Name {
		log.Printf("WARNING: Account %q attempted to handle server %s owned by %q.", account.Name, server.Name, server.Account)
		sendError(resp, errForbidden, "server cannot be managed by account %q", account.Name)
		return
	}

	switch req.Method {
	case "GET":
		result := map[string]interface{}{"server": server}
		sendJSON(resp, http.StatusOK, result)

	case "DELETE":
		if err = manager.Stop(server); err != nil {
			sendError(resp, errInternal, "cannot stop server %s: %s", server.Name, err)
			return
		}
		if err := manager.Deallocate(server); err != nil {
			sendError(resp, errInternal, "cannot deallocate server: %s", err)
			return
		}
		result := map[string]bool{"ok": true}
		sendJSON(resp, http.StatusOK, result)

	default:
		sendBadMethod(resp, req)
	}
}

func sendJSON(resp http.ResponseWriter, status int, value interface{}) {
	data, err := json.Marshal(value)
	if err != nil {
		data = []byte(`{"error": "cannot encode json response"}`)
	}
	resp.Header().Set("Content-Type", "application/json")
	resp.WriteHeader(status)
	resp.Write(data)
}

type (
	notFoundError struct{ error }
	requestError  struct{ error }
	authError     struct{ error }
	httpError     struct{ code int }
)

var (
	errInternal  = httpError{http.StatusInternalServerError}
	errNotFound  = httpError{http.StatusNotFound}
	errRequest   = httpError{http.StatusBadRequest}
	errAuth      = httpError{http.StatusUnauthorized}
	errForbidden = httpError{http.StatusForbidden}
)

func (e httpError) Error() string {
	return fmt.Sprintf("http error %d", e.code)
}

func sendError(resp http.ResponseWriter, err error, args ...interface{}) {
	type errorResult struct {
		Message string `json:"error"`
	}
	code := 500
	switch e := err.(type) {
	case notFoundError:
		code = http.StatusNotFound
	case requestError:
		code = http.StatusBadRequest
	case authError:
		code = http.StatusUnauthorized
	case httpError:
		code = e.code
	}
	var msg string
	switch len(args) {
	case 0:
		msg = err.Error()
	case 1:
		msg = fmt.Sprint(args[0])
	default:
		msg = fmt.Sprintf(fmt.Sprint(args[0]), args[1:]...)
	}
	sendJSON(resp, code, errorResult{msg})
}

func sendNotFound(resp http.ResponseWriter, req *http.Request) {
	sendError(resp, errNotFound, `path %s unsupported; see the documentation at github.com/snapcore/spread`, req.URL.Path)
}

func sendBadMethod(resp http.ResponseWriter, req *http.Request) {
	sendError(resp, errRequest, "method %s unsupported on %s", req.Method, req.URL.Path)
}
