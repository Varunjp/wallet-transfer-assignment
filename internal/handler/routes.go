package handler

import "github.com/gin-gonic/gin"

func RegisterRoutes(r *gin.Engine, h *TransferHandler) {
	r.POST("/api/transfers", h.CreateTransfer)
	r.POST("/transfers", h.CreateTransfer)
}
