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
	port := flag.String("port", ":8090", "HTTP listen address")
	dbPath := flag.String("db", "./vasuli.db", "SQLite database path")
	razorpayKeyID := flag.String("razorpay-key-id", "", "Razorpay test-mode key ID (empty uses the stub client)")
	razorpayKeySecret := flag.String("razorpay-key-secret", "", "Razorpay test-mode key secret")
	webhookSecret := flag.String("razorpay-webhook-secret", "", "Shared secret inbound webhooks are signed with")
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

	logInfo("Recovery Orchestrator listening on %s (db: %s, razorpay: %s)", *port, *dbPath, mode)
	if err := http.ListenAndServe(*port, router); err != nil {
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
