package handler

import (
	"net/http"

	"github.com/aarani/craftling-go/internal/logger"
	"github.com/aarani/craftling-go/internal/repository"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// AdminHandler serves admin-only endpoints.
type AdminHandler struct {
	users   *repository.UserRepository
	servers *repository.GameServerRepository
	hosts   *repository.HostRepository
}

// NewAdminHandler constructs an AdminHandler.
func NewAdminHandler(users *repository.UserRepository, servers *repository.GameServerRepository, hosts *repository.HostRepository) *AdminHandler {
	return &AdminHandler{users: users, servers: servers, hosts: hosts}
}

// ListUsers returns all users. Guarded by RequireRole(admin).
func (h *AdminHandler) ListUsers(c *gin.Context) {
	users, err := h.users.List(c.Request.Context())
	if err != nil {
		logger.FromContext(c).Error("list users", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"users": users})
}

// ListServers returns every server across all owners. Guarded by RequireRole(admin).
func (h *AdminHandler) ListServers(c *gin.Context) {
	servers, err := h.servers.ListAll(c.Request.Context())
	if err != nil {
		logger.FromContext(c).Error("list all servers", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"servers": servers})
}

// ListHosts returns the whole fleet inventory. Guarded by RequireRole(admin).
func (h *AdminHandler) ListHosts(c *gin.Context) {
	hosts, err := h.hosts.List(c.Request.Context())
	if err != nil {
		logger.FromContext(c).Error("list hosts", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"hosts": hosts})
}
