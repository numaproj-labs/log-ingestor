package logging

import (
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	// EnvVarDebugMode is the env variable for debug mode
	EnvVarDebugMode = "DEBUG_MODE"
)

// NewLogger returns a new zap sugared logger
func NewLogger() *zap.SugaredLogger {
	var config zap.Config
	debugMode, ok := os.LookupEnv(EnvVarDebugMode)
	if ok && debugMode == "true" {
		config = zap.NewDevelopmentConfig()
	} else {
		config = zap.NewProductionConfig()
	}
	// Config customization goes here if any
	config.EncoderConfig.EncodeTime = zapcore.RFC3339NanoTimeEncoder
	config.OutputPaths = []string{"stdout"}
	logger, err := config.Build()
	if err != nil {
		panic(err)
	}
	return logger.Named("log-ingestor").Sugar()
}
