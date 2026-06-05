package middleware

import (
	"net/http"
	"strings"

	"github.com/aarani/craftling-go/internal/auth"
	"github.com/gin-gonic/gin"
)

const (
	contextKeyUserID = "user_id"
	contextKeyEmail  = "email"
	contextKeyRole   = "role"
)

// Auth validates the Bearer token on the Authorization header. On success it
// stores the authenticated user's ID and email on the context; otherwise it
// aborts with 401.
func Auth(m *auth.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		token, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing or malformed bearer token"})
			return
		}

		claims, err := m.Parse(token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}

		c.Set(contextKeyUserID, claims.Subject)
		c.Set(contextKeyEmail, claims.Email)
		c.Set(contextKeyRole, claims.Role)
		c.Next()
	}
}

// RequireRole aborts with 403 unless the authenticated user has the given role.
// It must be registered after Auth, which populates the role on the context.
func RequireRole(role string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if RoleFromContext(c) != role {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
			return
		}
		c.Next()
	}
}

// UserIDFromContext returns the authenticated user's ID, if present.
func UserIDFromContext(c *gin.Context) string {
	return stringFromContext(c, contextKeyUserID)
}

// RoleFromContext returns the authenticated user's role, if present.
func RoleFromContext(c *gin.Context) string {
	return stringFromContext(c, contextKeyRole)
}

func stringFromContext(c *gin.Context, key string) string {
	if v, ok := c.Get(key); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
