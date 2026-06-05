package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// HeaderRequestID is the canonical header used to carry the request ID.
const HeaderRequestID = "X-Request-ID"

// contextKeyRequestID is the gin context key under which the request ID is stored.
const contextKeyRequestID = "request_id"

// RequestID assigns a unique ID to each request. It honors an incoming
// X-Request-ID header when present, otherwise generates a new UUID. The ID is
// stored in the gin context and echoed back on the response.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(HeaderRequestID)
		if id == "" {
			id = uuid.NewString()
		}

		c.Set(contextKeyRequestID, id)
		c.Header(HeaderRequestID, id)
		c.Next()
	}
}

// RequestIDFromContext returns the request ID stored on the gin context, if any.
func RequestIDFromContext(c *gin.Context) string {
	if v, ok := c.Get(contextKeyRequestID); ok {
		if id, ok := v.(string); ok {
			return id
		}
	}
	return ""
}
