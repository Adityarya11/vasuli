package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"vasuli/recovery-orchestrator/internal/api"
	"vasuli/recovery-orchestrator/internal/campaign"
	"vasuli/recovery-orchestrator/internal/razorpay"
	"vasuli/recovery-orchestrator/internal/store"
)

const timeFormat = "2006-01-02 15:04:05"
const moduleName = "Recovery"

func logInfo(format string, v ...any) {
	fmt.Printf("[%s] [INFO] [%s] %s\n", time.Now().Format(timeFormat), moduleName, fmt.Sprintf(format, v...))
}

func logWarn(format string, v ...any) {
	fmt.Printf("[%s] [WARNING] [%s] %s\n", time.Now().Format(timeFormat), moduleName, fmt.Sprintf(format, v...))
}

func logFatal(format string, v ...any) {
	fmt.Printf("[%s] [FATAL] [%s] %s\n", time.Now().Format(timeFormat), moduleName, fmt.Sprintf(format, v...))
	os.Exit(1)
}

func main() {
	// Loaded before the flags are declared, because each credential flag
	// reads its default from the environment at declaration time. That
	// gives precedence for free with no merge logic: an explicit flag
	// beats the shell environment, which beats the file.
	if err := loadDotEnv(".env"); err != nil {
		logFatal("Failed to load .env: %v", err)
	}

	port := flag.String("port", ":8090", "HTTP listen address")
	dbPath := flag.String("db", "./vasuli.db", "SQLite database path")
	razorpayKeyID := flag.String("razorpay-key-id", os.Getenv("RAZORPAY_KEY_ID"),
		"Razorpay test-mode key ID (env: RAZORPAY_KEY_ID; empty uses the stub client)")
	razorpayKeySecret := flag.String("razorpay-key-secret", os.Getenv("RAZORPAY_KEY_SECRET"),
		"Razorpay test-mode key secret (env: RAZORPAY_KEY_SECRET)")
	webhookSecret := flag.String("razorpay-webhook-secret", os.Getenv("RAZORPAY_WEBHOOK_SECRET"),
		"Shared secret inbound webhooks are signed with (env: RAZORPAY_WEBHOOK_SECRET)")
	flag.Parse()

	db, err := store.Open(*dbPath)
	if err != nil {
		logFatal("Failed to open database: %v", err)
	}
	defer db.Close()

	rzp, mode, err := buildRazorpayClient(*razorpayKeyID, *razorpayKeySecret)
	if err != nil {
		logFatal("%v", err)
	}

	manager, err := campaign.NewManager(db, rzp)
	if err != nil {
		logFatal("Failed to initialize campaign manager: %v", err)
	}

	if *webhookSecret == "" {
		logWarn("No -razorpay-webhook-secret set; inbound webhooks will be rejected.")
	}

	handlers := api.NewHandlers(manager, db, *webhookSecret)
	router := api.NewRouter(handlers)

	// http.ListenAndServe would leave every timeout at its zero value, which
	// means no timeout at all: a client that opens a connection and sends a
	// request slowly, or never finishes sending one, holds a goroutine and a
	// file descriptor for as long as it likes. The webhook endpoint is the
	// one an unauthenticated caller can reach, so the read side matters most.
	// The write timeout is generous because a payment-link call to Razorpay
	// happens inside the end-of-call request and is itself bounded at 10s.
	server := &http.Server{
		Addr:              *port,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	logInfo("Recovery Orchestrator listening on %s (db: %s, razorpay: %s)", *port, *dbPath, mode)
	if err := server.ListenAndServe(); err != nil {
		logFatal("Server failed: %v", err)
	}
}

// buildRazorpayClient selects the live client only when both credentials
// are present, and refuses to start against a live key.
//
// Vasuli generates payment links autonomously from an AI conversation.
// Pointing that at production credentials would create real payment demands
// against real customers, so the test-mode prefix is enforced at startup
// rather than trusted to whoever types the flags.
func buildRazorpayClient(keyID, keySecret string) (razorpay.Client, string, error) {
	if keyID == "" && keySecret == "" {
		return razorpay.NewStubClient(), "stub (no credentials supplied)", nil
	}

	if keyID == "" || keySecret == "" {
		return nil, "", fmt.Errorf(
			"razorpay credentials incomplete: -razorpay-key-id and -razorpay-key-secret must be set together",
		)
	}

	live := razorpay.NewLiveClient(keyID, keySecret)
	if !live.IsTestMode() {
		return nil, "", fmt.Errorf(
			"refusing to start: -razorpay-key-id %q is not a test-mode key (expected an rzp_test_ prefix)",
			keyID,
		)
	}

	return live, "live (test mode)", nil
}
