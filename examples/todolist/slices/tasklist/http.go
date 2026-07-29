package tasklist

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

type HTTPHandler struct {
	queryHandler *QueryHandler
}

func NewHTTPHandler(qh *QueryHandler) *HTTPHandler {
	return &HTTPHandler{queryHandler: qh}
}

func (h *HTTPHandler) Handle(c *gin.Context) {
	result, err := h.queryHandler.HandleQuery(c.Request.Context(), ListTasks{})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"tasks": result})
}

func (h *HTTPHandler) RegisterRoutes(g *gin.RouterGroup) {
	g.GET("", h.Handle)
}
