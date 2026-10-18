package createtask

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
)

// ReadModel is a read model the response waits for, so that a client that
// lists tasks right after creating one sees it. *projection.Runner
// implements it.
type ReadModel interface {
	WaitUntil(ctx context.Context, position uint64) error
}

type HTTPHandler struct {
	handler eventsourcing.Dispatcher
	list    ReadModel
}

func NewHTTPHandler(handler eventsourcing.Dispatcher, list ReadModel) *HTTPHandler {
	return &HTTPHandler{handler: handler, list: list}
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

	// Read your writes: answer once the task list has the new task.
	if err := h.list.WaitUntil(c.Request.Context(), result.GlobalPosition); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"task_id":         cmd.TaskID,
		"version":         result.NextExpectedVersion,
		"global_position": result.GlobalPosition,
	})
}

func (h *HTTPHandler) RegisterRoutes(g *gin.RouterGroup) {
	g.POST("", h.Handle)
}
