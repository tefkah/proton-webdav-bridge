package main

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	drive "github.com/StollD/proton-drive"
	"github.com/StollD/webdav"
	"github.com/adrg/xdg"
	"gitlab.com/david_mbuvi/go_asterisks"
)

const (
	TokenFile  = "proton-webdav-bridge/tokens.json"
	AppVersion = "macos-drive@1.0.0-alpha.1+proton-webdav-bridge"
)

var (
	OptLogin       = false
	OptListen      = "127.0.0.1:7984"
	OptAdminListen = "127.0.0.1:7985"
	authStatus     = &AuthStatus{LoggedIn: false}
	webdavServer   *http.Server
	webdavCancel   context.CancelFunc
	webdavMutex    sync.Mutex
)

// embed static files
//go:embed static
var staticFiles embed.FS

// authStatus keeps track of the current authentication state
type AuthStatus struct {
	LoggedIn    bool      `json:"logged_in"`
	LastLogin   time.Time `json:"last_login,omitempty"`
	NeedsLogin  bool      `json:"needs_login"`
	Error       string    `json:"error,omitempty"`
	mu          sync.Mutex
}

// loginRequest represents login form data
type loginRequest struct {
	Username        string `json:"username"`
	Password        string `json:"password"`
	MailboxPassword string `json:"mailbox_password"`
	TwoFA           string `json:"twofa"`
}

// get credential from environment or prompt user
func getCredential(envVar, prompt, hint string, masked bool) (string, error) {
	// try to get from environment first
	if value := os.Getenv(envVar); value != "" {
		// special case: "false" for optional credentials means skip/empty
		if value == "false" && (envVar == "PROTON_MAILBOX_PASSWORD" || envVar == "PROTON_2FA") {
			return "", nil
		}
		return value, nil
	}

	// if not in environment, prompt user
	reader := bufio.NewReader(os.Stdin)
	fmt.Println(prompt)
	if hint != "" {
		fmt.Println(hint)
	}
	fmt.Print("> ")

	if masked {
		pass, err := go_asterisks.GetUsersPassword("", true, os.Stdin, os.Stdout)
		fmt.Println()
		return string(pass), err
	}

	value, err := reader.ReadString('\n')
	fmt.Println()
	return strings.TrimSpace(value), err
}

func doLogin() error {
	user, err := getCredential("PROTON_USERNAME", "Enter the username of your Proton Drive account.", "", false)
	if err != nil {
		return err
	}

	pass, err := getCredential("PROTON_PASSWORD", "Enter the password of your Proton Drive account.", "", true)
	if err != nil {
		return err
	}

	mailbox, err := getCredential("PROTON_MAILBOX_PASSWORD", "Enter the mailbox password of your Proton Drive account.", "If you don't have a mailbox password, press enter.", true)
	if err != nil {
		return err
	}

	twoFA, err := getCredential("PROTON_2FA", "Enter a valid 2FA token for your Proton Drive account.", "If you don't have 2FA setup, press enter.", false)
	if err != nil {
		return err
	}

	return loginWithCredentials(user, pass, mailbox, twoFA)
}

func loginWithCredentials(username, password, mailboxPassword, twoFA string) error {
	credentials := drive.Credentials{
		Username:        username,
		Password:        password,
		MailboxPassword: mailboxPassword,
		TwoFA:           twoFA,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app := drive.NewApplication(AppVersion)
	err := app.LoginWithCredentials(ctx, credentials)
	if err != nil {
		authStatus.mu.Lock()
		authStatus.LoggedIn = false
		authStatus.NeedsLogin = true
		authStatus.Error = err.Error()
		authStatus.mu.Unlock()
		return err
	}

	err = storeTokens(*app.Tokens())
	if err != nil {
		return err
	}

	authStatus.mu.Lock()
	authStatus.LoggedIn = true
	authStatus.LastLogin = time.Now()
	authStatus.NeedsLogin = false
	authStatus.Error = ""
	authStatus.mu.Unlock()

	fmt.Println("Login successful.")
	
	// Start the WebDAV server with the new tokens
	go startWebDAVServer()
	
	return nil
}

func canAutoLogin() bool {
	return os.Getenv("PROTON_USERNAME") != "" && 
	       os.Getenv("PROTON_PASSWORD") != "" 
}

func doListen() error {
	// Always start the admin server first
	go startAdminServer()
	
	tokens, err := loadTokens()
	autoLoginAvailable := canAutoLogin()
	
	if err != nil {
		authStatus.mu.Lock()
		authStatus.LoggedIn = false
		authStatus.NeedsLogin = true
		authStatus.Error = "No valid tokens found"
		authStatus.mu.Unlock()
		
		fmt.Println("Failed to load tokens!")
		fmt.Println("Use the web UI to login or set environment variables.")
		fmt.Println(fmt.Sprintf("Admin interface available at http://%s", OptAdminListen))
		
		if autoLoginAvailable {
			// Auto-login using environment variables
			fmt.Println("Attempting automatic login with environment variables...")
			if err := doLogin(); err != nil {
				fmt.Println("Automatic login failed:", err)
				// Wait indefinitely - admin server is running
				waitForever()
				return nil
			}
		} else {
			// Wait indefinitely - admin server is running
			waitForever()
			return nil
		}
	} else if tokens.AccessToken != "" {
		// We have tokens, start the WebDAV server
		authStatus.mu.Lock()
		authStatus.LoggedIn = true
		authStatus.LastLogin = time.Now()
		authStatus.NeedsLogin = false
		authStatus.mu.Unlock()
		
		// Start WebDAV server with existing tokens
		go startWebDAVServer()
	}

	// Wait indefinitely - both servers are running
	waitForever()
	return nil
}

// startWebDAVServer starts the WebDAV server with current tokens
func startWebDAVServer() {
	webdavMutex.Lock()
	defer webdavMutex.Unlock()
	
	// Stop the existing server if it's running
	if webdavServer != nil {
		stopWebDAVServer()
	}
	
	tokens, err := loadTokens()
	if err != nil {
		fmt.Println("Error loading tokens:", err)
		return
	}
	
	fmt.Println("Waiting for network ...")
	WaitNetwork()

	fmt.Println("Connecting to Proton Drive ...")

	// Create a context that can be canceled when we need to stop the server
	var ctx context.Context
	ctx, webdavCancel = context.WithCancel(context.Background())

	app := drive.NewApplication(AppVersion)
	app.LoginWithTokens(&tokens)

	app.OnTokensUpdated(func(tokens *drive.Tokens) {
		err := storeTokens(*tokens)
		if err == nil {
			return
		}

		fmt.Println("Error storing tokens:", err)
	})

	app.OnTokensExpired(func() {
		fmt.Println("Tokens expired!")
		
		authStatus.mu.Lock()
		authStatus.LoggedIn = false
		authStatus.NeedsLogin = true
		authStatus.Error = "Tokens expired"
		authStatus.mu.Unlock()
		
		// Stop the WebDAV server since tokens are expired
		stopWebDAVServer()
		
		if canAutoLogin() {
			fmt.Println("Attempting to renew tokens with environment variables...")
			if err := doLogin(); err != nil {
				fmt.Println("Error renewing tokens:", err)
			}
		} else {
			fmt.Println("Please login via the web UI to renew tokens.")
		}
	})

	session := drive.NewSession(app)

	err = session.Init(ctx)
	if err != nil {
		fmt.Println("Error initializing session:", err)
		return
	}

	fmt.Println("Connected!")
	fmt.Println(fmt.Sprintf("WebDAV server available at http://%s", OptListen))

	webdavServer = &http.Server{
		Addr: OptListen,
		Handler: &webdav.Handler{
			FileSystem: &ProtonFS{session: session},
			LockSystem: webdav.NewMemLS(),
		},
	}
	
	// Start the server in a goroutine
	go func() {
		err := webdavServer.ListenAndServe()
		if err != http.ErrServerClosed {
			fmt.Printf("WebDAV server error: %v\n", err)
		}
	}()
}

// stopWebDAVServer gracefully stops the WebDAV server
func stopWebDAVServer() {
	if webdavServer == nil {
		return
	}
	
	fmt.Println("Stopping WebDAV server...")
	
	// Cancel the context to stop any ongoing operations
	if webdavCancel != nil {
		webdavCancel()
	}
	
	// Create a shutdown context with a timeout
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	
	err := webdavServer.Shutdown(ctx)
	if err != nil {
		fmt.Printf("Error shutting down WebDAV server: %v\n", err)
	}
	
	webdavServer = nil
	fmt.Println("WebDAV server stopped.")
}

// waitForever blocks indefinitely, keeping the main goroutine alive
func waitForever() {
	select {} // This will block forever
}

func startAdminServer() {
	mux := http.NewServeMux()
	
	// API endpoints
	mux.HandleFunc("/api/status", handleStatus)
	mux.HandleFunc("/api/login", handleLogin)
	mux.HandleFunc("/api/logout", handleLogout)
	
	// Serve static files
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		fmt.Println("Error setting up static file server:", err)
		return
	}
	fileServer := http.FileServer(http.FS(sub))
	mux.Handle("/", fileServer)
	
	fmt.Printf("Admin interface available at http://%s\n", OptAdminListen)
	err = http.ListenAndServe(OptAdminListen, mux)
	if err != nil {
		fmt.Printf("Admin server error: %v\n", err)
	}
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	
	authStatus.mu.Lock()
	defer authStatus.mu.Unlock()
	
	err := json.NewEncoder(w).Encode(authStatus)
	if err != nil {
		http.Error(w, "Error encoding response", http.StatusInternalServerError)
	}
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	
	err := loginWithCredentials(req.Username, req.Password, req.MailboxPassword, req.TwoFA)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	// Stop WebDAV server
	webdavMutex.Lock()
	if webdavServer != nil {
		stopWebDAVServer()
	}
	webdavMutex.Unlock()
	
	// Delete tokens file
	file, err := xdg.DataFile(TokenFile)
	if err == nil {
		os.Remove(file)
	}
	
	authStatus.mu.Lock()
	authStatus.LoggedIn = false
	authStatus.NeedsLogin = true
	authStatus.Error = ""
	authStatus.mu.Unlock()
	
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func loadTokens() (drive.Tokens, error) {
	var tokens drive.Tokens

	file, err := xdg.DataFile(TokenFile)
	if err != nil {
		return tokens, err
	}

	enc, err := os.ReadFile(file)
	if err != nil {
		return tokens, err
	}

	err = json.Unmarshal(enc, &tokens)
	if err != nil {
		return tokens, err
	}

	return tokens, nil
}

func storeTokens(tokens drive.Tokens) error {
	file, err := xdg.DataFile(TokenFile)
	if err != nil {
		return err
	}

	// Ensure the directory exists
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	enc, err := json.Marshal(tokens)
	if err != nil {
		return err
	}

	return os.WriteFile(file, enc, 0600)
}

func main() {
	var err error = nil

	flag.BoolVar(&OptLogin, "login", OptLogin, "Run Proton Drive login")
	flag.StringVar(&OptListen, "listen", OptListen, "Which address the WebDAV server will listen to")
	flag.StringVar(&OptAdminListen, "admin-listen", OptAdminListen, "Which address the admin interface will listen to")
	flag.Parse()

	if OptLogin {
		err = doLogin()
	} else {
		err = doListen()
	}

	if err != nil {
		panic(err)
	}
}
