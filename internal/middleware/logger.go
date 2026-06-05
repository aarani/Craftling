package middleware

import (
	"time"

	"github.com/aarani/craftling-go/internal/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// RequestLogger derives a request-scoped logger (tagged with the request ID),
// makes it available to downstream handlers via the context, and logs a
// summary line once the request completes.
func RequestLogger(log *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		reqLog := log.With(zap.String("request_id", RequestIDFromContext(c)))
		logger.IntoContext(c, reqLog)

		c.Next()

		reqLog.Info("request",
			zap.String("method", c.Request.Method),
			zap.String("path", c.Request.URL.Path),
			zap.Int("status", c.Writer.Status()),
			zap.Duration("latency", time.Since(start)),
			zap.String("client_ip", c.ClientIP()),
		)
	}
}
