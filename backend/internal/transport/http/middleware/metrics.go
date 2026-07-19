package middleware

import (
	"time"

	"github.com/chenyme/grok2api/backend/internal/observability"
	"github.com/gin-gonic/gin"
)

func Metrics() gin.HandlerFunc {
	return func(c *gin.Context) {
		startedAt := time.Now()
		c.Next()
		observability.ObserveHTTP(c.FullPath(), c.Request.Method, c.Writer.Status(), time.Since(startedAt))
	}
}
