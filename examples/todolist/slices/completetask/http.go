package completetask

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

	if _, err := h.handler.Dispatch(c.Request.Context(), cmd); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.Status(http.StatusNoContent)
}

func (h *HTTPHandler) RegisterRoutes(g *gin.RouterGroup) {
	g.POST("/:taskID/complete", h.Handle)
}
