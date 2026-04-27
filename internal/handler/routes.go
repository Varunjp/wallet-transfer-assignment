package handler

import "github.com/gin-gonic/gin"

func RegisterRoutes(r *gin.Engine, h *TransferHandler) {
	api := r.Group("/api")
	api.POST("/transfers",h.CreateTransfer)
}