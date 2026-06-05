package handler

import (
	"net/http"

	"github.com/aarani/craftling-go/internal/logger"
	"github.com/gin-gonic/gin"
)

// Health reports service liveness.
func Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// Ping is a simple example endpoint. It pulls the request-scoped logger from
// the context, so its log line is automatically tagged with the request ID.
func Ping(c *gin.Context) {
	logger.FromContext(c).Info("handling ping")
	c.JSON(http.StatusOK, gin.H{"message": "pong"})
}
