package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
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
	TokenFile        = "proton-webdav-bridge/tokens.json"
	AdminPasswordFile = "proton-webdav-bridge/admin_password.json"
	AppVersion       = "macos-drive@1.0.0-alpha.1+proton-webdav-bridge"
)

var (
	OptLogin       = false
	OptListen      = "127.0.0.1:7984"
	OptAdminListen = "127.0.0.1:7985"
	authStatus     = &AuthStatus{LoggedIn: false}
	webdavServer   *http.Server
	webdavCancel   context.CancelFunc
	webdavMutex    sync.Mutex
	adminAuth      = &AdminAuth{initialized: false}
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

// AdminAuth keeps track of admin authentication
type AdminAuth struct {
	initialized bool
	passwordHash string
	salt string
	sessions map[string]time.Time
	mu sync.Mutex
}

// AdminPasswordData represents stored password data
type AdminPasswordData struct {
	PasswordHash string `json:"password_hash"`
	Salt         string `json:"salt"`
}

// loginRequest represents login form data
type loginRequest struct {
	Username        string `json:"username"`
	Password        string `json:"password"`
	MailboxPassword string `json:"mailbox_password"`
	TwoFA           string `json:"twofa"`
}

// adminLoginRequest represents admin login data
type adminLoginRequest struct {
	Password string `json:"password"`
}

// adminSetupRequest represents admin setup data
type adminSetupRequest struct {
	Password string `json:"password"`
}

// adminStatusResponse represents admin status
type adminStatusResponse struct {
	Initialized bool `json:"initialized"`
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
	// Check for admin password reset
	if os.Getenv("ADMIN_PASSWORD_RESET") == "true" {
		fmt.Println("Admin password reset requested, removing password file...")
		resetAdminPassword()
	}

	// Initialize admin auth
	initAdminAuth()

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

// initialize admin auth system
func initAdminAuth() {
	adminAuth.mu.Lock()
	defer adminAuth.mu.Unlock()

	// Initialize sessions map
	adminAuth.sessions = make(map[string]time.Time)

	// Try to load existing password data
	data, err := loadAdminPassword()
	if err != nil {
		// No password set yet, will show setup screen
		adminAuth.initialized = false
		return
	}

	// Password found, initialize auth system
	adminAuth.passwordHash = data.PasswordHash
	adminAuth.salt = data.Salt
	adminAuth.initialized = true
}

// resetAdminPassword deletes the admin password file to reset it
func resetAdminPassword() {
	file, err := xdg.DataFile(AdminPasswordFile)
	if err == nil {
		os.Remove(file)
		fmt.Println("Admin password has been reset.")
	}
	
	// Reset the in-memory state
	adminAuth.mu.Lock()
	adminAuth.initialized = false
	adminAuth.passwordHash = ""
	adminAuth.salt = ""
	adminAuth.sessions = make(map[string]time.Time)
	adminAuth.mu.Unlock()
}

// loadAdminPassword loads the admin password data
func loadAdminPassword() (AdminPasswordData, error) {
	var data AdminPasswordData

	file, err := xdg.DataFile(AdminPasswordFile)
	if err != nil {
		return data, err
	}

	enc, err := os.ReadFile(file)
	if err != nil {
		return data, err
	}

	err = json.Unmarshal(enc, &data)
	if err != nil {
		return data, err
	}

	return data, nil
}

// storeAdminPassword saves the admin password data
func storeAdminPassword(data AdminPasswordData) error {
	file, err := xdg.DataFile(AdminPasswordFile)
	if err != nil {
		return err
	}

	// Ensure the directory exists
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	enc, err := json.Marshal(data)
	if err != nil {
		return err
	}

	return os.WriteFile(file, enc, 0600)
}

// generateSalt creates a random salt for password hashing
func generateSalt() (string, error) {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// hashPassword creates a salted hash of the password
func hashPassword(password, salt string) string {
	hash := sha256.New()
	hash.Write([]byte(password + salt))
	return hex.EncodeToString(hash.Sum(nil))
}

// generateSessionToken creates a new session token
func generateSessionToken() (string, error) {
	b := make([]byte, 32)
	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
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
	
	// Protected API endpoints
	mux.HandleFunc("/api/status", withAdminAuth(handleStatus))
	mux.HandleFunc("/api/login", withAdminAuth(handleLogin))
	mux.HandleFunc("/api/logout", withAdminAuth(handleLogout))
	
	// Admin auth endpoints
	mux.HandleFunc("/api/admin/status", handleAdminStatus)
	mux.HandleFunc("/api/admin/setup", handleAdminSetup)
	mux.HandleFunc("/api/admin/login", handleAdminLogin)
	mux.HandleFunc("/api/admin/logout", handleAdminLogout)
	
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

// withAdminAuth wraps a handler with admin authentication
func withAdminAuth(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// If not initialized yet, allow access
		adminAuth.mu.Lock()
		initialized := adminAuth.initialized
		adminAuth.mu.Unlock()
		
		if !initialized {
			handler(w, r)
			return
		}
		
		// Check for session cookie
		cookie, err := r.Cookie("admin_session")
		if err != nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		
		// Validate session
		adminAuth.mu.Lock()
		expiry, exists := adminAuth.sessions[cookie.Value]
		adminAuth.mu.Unlock()
		
		if !exists || time.Now().After(expiry) {
			http.Error(w, "Session expired", http.StatusUnauthorized)
			return
		}
		
		// Session valid, execute handler
		handler(w, r)
	}
}

func handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	
	adminAuth.mu.Lock()
	initialized := adminAuth.initialized
	adminAuth.mu.Unlock()
	
	status := adminStatusResponse{
		Initialized: initialized,
	}
	
	err := json.NewEncoder(w).Encode(status)
	if err != nil {
		http.Error(w, "Error encoding response", http.StatusInternalServerError)
	}
}

func handleAdminSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	// Check if already initialized
	adminAuth.mu.Lock()
	initialized := adminAuth.initialized
	adminAuth.mu.Unlock()
	
	if initialized {
		http.Error(w, "Admin already initialized", http.StatusBadRequest)
		return
	}
	
	// Parse request
	var req adminSetupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	
	// Validate password
	if len(req.Password) < 8 {
		http.Error(w, "Password must be at least 8 characters", http.StatusBadRequest)
		return
	}
	
	// Generate salt
	salt, err := generateSalt()
	if err != nil {
		http.Error(w, "Error generating salt", http.StatusInternalServerError)
		return
	}
	
	// Hash password
	passwordHash := hashPassword(req.Password, salt)
	
	// Store password data
	data := AdminPasswordData{
		PasswordHash: passwordHash,
		Salt:         salt,
	}
	
	if err := storeAdminPassword(data); err != nil {
		http.Error(w, "Error storing password", http.StatusInternalServerError)
		return
	}
	
	// Update in-memory state
	adminAuth.mu.Lock()
	adminAuth.passwordHash = passwordHash
	adminAuth.salt = salt
	adminAuth.initialized = true
	adminAuth.mu.Unlock()
	
	// Generate session token
	token, err := generateSessionToken()
	if err != nil {
		http.Error(w, "Error generating session", http.StatusInternalServerError)
		return
	}
	
	// Store session
	expiry := time.Now().Add(24 * time.Hour)
	adminAuth.mu.Lock()
	adminAuth.sessions[token] = expiry
	adminAuth.mu.Unlock()
	
	// Set session cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_session",
		Value:    token,
		Path:     "/",
		Expires:  expiry,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	// Check if initialized
	adminAuth.mu.Lock()
	initialized := adminAuth.initialized
	passwordHash := adminAuth.passwordHash
	salt := adminAuth.salt
	adminAuth.mu.Unlock()
	
	if !initialized {
		http.Error(w, "Admin not initialized", http.StatusBadRequest)
		return
	}
	
	// Parse request
	var req adminLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	
	// Validate password
	if hashPassword(req.Password, salt) != passwordHash {
		http.Error(w, "Invalid password", http.StatusUnauthorized)
		return
	}
	
	// Generate session token
	token, err := generateSessionToken()
	if err != nil {
		http.Error(w, "Error generating session", http.StatusInternalServerError)
		return
	}
	
	// Store session
	expiry := time.Now().Add(24 * time.Hour)
	adminAuth.mu.Lock()
	adminAuth.sessions[token] = expiry
	adminAuth.mu.Unlock()
	
	// Set session cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_session",
		Value:    token,
		Path:     "/",
		Expires:  expiry,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	// Clear session cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_session",
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	
	// Remove session from memory if it exists
	cookie, err := r.Cookie("admin_session")
	if err == nil {
		adminAuth.mu.Lock()
		delete(adminAuth.sessions, cookie.Value)
		adminAuth.mu.Unlock()
	}
	
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
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
