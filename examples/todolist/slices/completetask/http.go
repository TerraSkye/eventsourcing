package completetask

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
)

// ReadModel is a read model the response waits for, so that a client that
// lists tasks right after completing one sees it completed.
// *projection.Runner implements it.
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

func (h *HTTPHandler) Handle(c *gin.Context) {
	taskID, err := uuid.Parse(c.Param("taskID"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task ID"})
		return
	}

	cmd := CompleteTask{
		TaskID:      taskID,
		CompletedBy: uuid.New(), // replace with user from auth context
	}

	result, err := h.handler.Dispatch(c.Request.Context(), cmd)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Read your writes: answer once the task list shows the task completed.
	if err := h.list.WaitUntil(c.Request.Context(), result.GlobalPosition); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.Status(http.StatusNoContent)
}

func (h *HTTPHandler) RegisterRoutes(g *gin.RouterGroup) {
	g.POST("/:taskID/complete", h.Handle)
}
