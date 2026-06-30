package logger

import (
	"io"
	"log"
	"os"
	"strings"
)

var (
	Debug = log.New(io.Discard, "", 0)
	Info  = log.New(io.Discard, "", 0)
	Warn  = log.New(io.Discard, "", 0)
	Error = log.New(io.Discard, "", 0)
)

// Init initializes the global loggers based on the provided log level.
func Init(level string) {
	level = strings.ToLower(level)
	if level == "" {
		level = "info" // Default level
	}

	out := os.Stdout
	errOut := os.Stderr
	flags := log.LstdFlags

	// Reset to discard first
	Debug = log.New(io.Discard, "", 0)
	Info = log.New(io.Discard, "", 0)
	Warn = log.New(io.Discard, "", 0)
	Error = log.New(io.Discard, "", 0)

	switch level {
	case "debug":
		Debug = log.New(out, "[DEBUG] ", flags)
		fallthrough
	case "info":
		Info = log.New(out, "[INFO] ", flags)
		fallthrough
	case "warn":
		Warn = log.New(errOut, "[WARN] ", flags)
		fallthrough
	case "error":
		Error = log.New(errOut, "[ERROR] ", flags)
	}
}
