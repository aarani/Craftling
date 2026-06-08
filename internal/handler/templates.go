package handler

import (
	"errors"
	"net/http"

	"github.com/aarani/craftling-go/internal/logger"
	"github.com/aarani/craftling-go/internal/registry"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// TemplateHandler serves the game-server template registry (the marketplace),
// proxying the upstream index and manifests through the control plane.
type TemplateHandler struct {
	registry *registry.Client
}

// NewTemplateHandler constructs a TemplateHandler over the given registry client.
func NewTemplateHandler(r *registry.Client) *TemplateHandler {
	return &TemplateHandler{registry: r}
}

// List returns the registry index: the templates available to launch from.
func (h *TemplateHandler) List(c *gin.Context) {
	templates, err := h.registry.Index(c.Request.Context())
	if err != nil {
		logger.FromContext(c).Error("fetch template index", zap.Error(err))
		c.JSON(http.StatusBadGateway, gin.H{"error": "could not reach the template registry"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"templates": templates})
}

// Get returns the full manifest for a single template by id. The raw upstream
// JSON is passed through verbatim so no fields are dropped.
func (h *TemplateHandler) Get(c *gin.Context) {
	body, err := h.registry.Manifest(c.Request.Context(), c.Param("id"))
	if errors.Is(err, registry.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "template not found"})
		return
	}
	if err != nil {
		logger.FromContext(c).Error("fetch template manifest", zap.Error(err))
		c.JSON(http.StatusBadGateway, gin.H{"error": "could not reach the template registry"})
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}
