package createtask

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
)

type HTTPHandler struct {
	handler eventsourcing.Dispatcher
}

func NewHTTPHandler(handler eventsourcing.Dispatcher) *HTTPHandler {
	return &HTTPHandler{handler: handler}
}

type request struct {
	Title       string `json:"title" binding:"required"`
	Description string `json:"description"`
}

func (h *HTTPHandler) Handle(c *gin.Context) {
	var req request
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	cmd := CreateTask{
		TaskID:      uuid.New(),
		Title:       req.Title,
		Description: req.Description,
		CreatedBy:   uuid.New(), // replace with user from auth context
	}

	result, err := h.handler.Dispatch(c.Request.Context(), cmd)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"task_id": cmd.TaskID,
		"version": result.NextExpectedVersion,
	})
}

func (h *HTTPHandler) RegisterRoutes(g *gin.RouterGroup) {
	g.POST("", h.Handle)
}
