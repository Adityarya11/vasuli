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

func logFatal(format string, v ...any) {
	fmt.Printf("[%s] [FATAL] [%s] %s\n", time.Now().Format(timeFormat), moduleName, fmt.Sprintf(format, v...))
	os.Exit(1)
}

func main() {
	port := flag.String("port", ":8090", "HTTP listen address")
	dbPath := flag.String("db", "./vasuli.db", "SQLite database path")
	flag.Parse()

	db, err := store.Open(*dbPath)
	if err != nil {
		logFatal("Failed to open database: %v", err)
	}
	defer db.Close()

	// M6 will swap this for a Live client once test-mode credentials
	// exist. See internal/razorpay/client.go for why this is an interface.
	rzp := razorpay.NewStubClient()

	manager, err := campaign.NewManager(db, rzp)
	if err != nil {
		logFatal("Failed to initialize campaign manager: %v", err)
	}

	handlers := api.NewHandlers(manager, db)
	router := api.NewRouter(handlers)

	logInfo("Recovery Orchestrator listening on %s (db: %s, razorpay: stub)", *port, *dbPath)
	if err := http.ListenAndServe(*port, router); err != nil {
		logFatal("Server failed: %v", err)
	}
}
