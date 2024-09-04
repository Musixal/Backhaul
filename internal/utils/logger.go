package utils

import (
	"fmt"
	"os"
	"strings"

	"github.com/sirupsen/logrus"
)

type CustomFormatter struct{}

func (f *CustomFormatter) Format(entry *logrus.Entry) ([]byte, error) {
	timestamp := entry.Time.Format("02-Jan 15:04:05")

	level := strings.ToUpper(entry.Level.String())
	coloredLevel := f.colorize(entry.Level, level)

	logMessage := fmt.Sprintf("%s [%s] %s\n", timestamp, coloredLevel, entry.Message)

	return []byte(logMessage), nil
}

// colorize the output
func (f *CustomFormatter) colorize(level logrus.Level, text string) string {
	switch level {
	case logrus.PanicLevel:
		return "\033[1;31m" + text + "\033[0m" // Bright Red for PANIC
	case logrus.FatalLevel:
		return "\033[1;31m" + text + "\033[0m" // Bright Red for FATAL
	case logrus.ErrorLevel:
		return "\033[31m" + text + "\033[0m" // Red for ERROR
	case logrus.WarnLevel:
		return "\033[33m" + text + "\033[0m" // Yellow for WARN
	case logrus.InfoLevel:
		return "\033[32m" + text + "\033[0m" // Green for INFO
	case logrus.DebugLevel:
		return "\033[34m" + text + "\033[0m" // Blue for DEBUG
	case logrus.TraceLevel:
		return "\033[36m" + text + "\033[0m" // Cyan for TRACE
	default:
		return text
	}
}

func NewLogger(logLevel string) *logrus.Logger {
	log := logrus.New()

	// Select log level
	log.SetOutput(os.Stdout)

	// parsed in the config.go already. so err is nil
	parseLevel, _ := logrus.ParseLevel(logLevel)

	log.SetLevel(parseLevel)

	log.SetFormatter(&CustomFormatter{})

	return log
}
