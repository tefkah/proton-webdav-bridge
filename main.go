package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"

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
	OptLogin  = false
	OptListen = "127.0.0.1:7984"
)

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

	credentials := drive.Credentials{
		Username:        user,
		Password:        pass,
		MailboxPassword: mailbox,
		TwoFA:           twoFA,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app := drive.NewApplication(AppVersion)
	err = app.LoginWithCredentials(ctx, credentials)
	if err != nil {
		return err
	}

	err = storeTokens(*app.Tokens())
	if err != nil {
		return err
	}

	fmt.Println("Login successful.")
	return nil
}

func canAutoLogin() bool {
	return os.Getenv("PROTON_USERNAME") != "" && 
	       os.Getenv("PROTON_PASSWORD") != "" 
}

func doListen() error {
	tokens, err := loadTokens()
	autoLoginAvailable := canAutoLogin()
	
	if err != nil {
		if !autoLoginAvailable {
			fmt.Println("Failed to load tokens!")
			fmt.Println("Run with --login to fix this or set environment variables.")
			fmt.Println()
			return err
		}
		
		// Auto-login using environment variables
		fmt.Println("No tokens found, attempting automatic login...")
		if err := doLogin(); err != nil {
			return err
		}
		
		// Reload tokens after login
		tokens, err = loadTokens()
		if err != nil {
			return err
		}
	}

	fmt.Println("Waiting for network ...")
	WaitNetwork()

	fmt.Println("Connecting to Proton Drive ...")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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
		fmt.Println("Tokens expired, attempting to renew...")
		
		if !autoLoginAvailable {
			fmt.Println("Unable to automatically renew tokens!")
			fmt.Println("Run with --login to fix this or set environment variables.")
			fmt.Println()
			os.Exit(1)
		}
		
		// Auto re-login
		if err := doLogin(); err != nil {
			fmt.Println("Error renewing tokens:", err)
			os.Exit(1)
		}
		
		fmt.Println("Tokens renewed, restarting connection...")
		// We exit and let the process manager restart us
		os.Exit(0)
	})

	session := drive.NewSession(app)

	err = session.Init(ctx)
	if err != nil {
		return err
	}

	fmt.Println("Connected!")

	return http.ListenAndServe(OptListen, &webdav.Handler{
		FileSystem: &ProtonFS{session: session},
		LockSystem: webdav.NewMemLS(),
	})
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
