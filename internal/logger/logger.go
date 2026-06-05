package logger

import (
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// contextKey is the gin context key under which the request-scoped logger is stored.
const contextKey = "logger"

// New builds a zap logger appropriate for the given environment.
// "production" yields JSON output at info level; anything else yields
// human-friendly console output at debug level.
func New(env string) (*zap.Logger, error) {
	if env == "production" {
		return zap.NewProduction()
	}
	return zap.NewDevelopment()
}

// IntoContext stores a request-scoped logger on the gin context.
func IntoContext(c *gin.Context, log *zap.Logger) {
	c.Set(contextKey, log)
}

// FromContext retrieves the request-scoped logger from the gin context,
// falling back to the global zap logger if none was set.
func FromContext(c *gin.Context) *zap.Logger {
	if v, ok := c.Get(contextKey); ok {
		if log, ok := v.(*zap.Logger); ok {
			return log
		}
	}
	return zap.L()
}
