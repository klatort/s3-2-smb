package log

import (
	"fmt"
	"log"
	"os"
	"time"
)

// Log levels
const (
	LevelDebug = "DEBUG"
	LevelInfo  = "INFO"
	LevelWarn  = "WARN"
	LevelError = "ERROR"
)

var (
	debugEnabled = false
	logger       = log.New(os.Stderr, "", 0)
)

// EnableDebug enables debug logging
func EnableDebug() {
	debugEnabled = true
}

// DisableDebug disables debug logging
func DisableDebug() {
	debugEnabled = false
}

// SetLogger sets a custom logger
func SetLogger(l *log.Logger) {
	logger = l
}

// Debug logs a debug message
func Debug(format string, args ...interface{}) {
	if debugEnabled {
		logMessage(LevelDebug, format, args...)
	}
}

// Info logs an info message
func Info(format string, args ...interface{}) {
	logMessage(LevelInfo, format, args...)
}

// Warn logs a warning message
func Warn(format string, args ...interface{}) {
	logMessage(LevelWarn, format, args...)
}

// Error logs an error message
func Error(format string, args ...interface{}) {
	logMessage(LevelError, format, args...)
}

// logMessage formats and logs a message
func logMessage(level, format string, args ...interface{}) {
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	message := fmt.Sprintf(format, args...)
	logger.Printf("[%s] %s: %s", timestamp, level, message)
}

// Fatal logs an error message and exits
func Fatal(format string, args ...interface{}) {
	logMessage(LevelError, format, args...)
	os.Exit(1)
}

// WithError logs an error with additional context
func WithError(err error, format string, args ...interface{}) {
	if err != nil {
		args = append(args, err)
		logMessage(LevelError, format+": %v", args...)
	}
}