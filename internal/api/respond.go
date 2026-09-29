package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/nginxui/plugin-log-analytics/internal/apierr"
	"github.com/nginxui/plugin-log-analytics/internal/logger"
)

// errorBody is the JSON of an error that carries no code of its own.
type errorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// validateErrorBody is the JSON of a request body that could not be read.
type validateErrorBody struct {
	Scope   string         `json:"scope"`
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Errors  map[string]any `json:"errors"`
}

// writeError answers with the error. A coded error keeps its scope, code,
// message and parameters, which is what clients translate. Any other error
// becomes a 500 with its text, and a missing record a 404.
func writeError(c *gin.Context, err error) {
	logger.Errorf("%s %s: %v", c.Request.Method, c.Request.URL.Path, err)

	var coded *apierr.Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		c.JSON(http.StatusNotFound, errorBody{Code: http.StatusNotFound, Message: gorm.ErrRecordNotFound.Error()})
	case errors.As(err, &coded):
		c.JSON(http.StatusInternalServerError, coded)
	default:
		c.JSON(http.StatusInternalServerError, errorBody{Code: http.StatusInternalServerError, Message: err.Error()})
	}
}

// bindAndValid reads the JSON body into target. When the body cannot be read it
// answers 406 with the same shape the host used for a validation failure, and
// reports false.
func bindAndValid(c *gin.Context, target any) bool {
	if err := c.ShouldBindJSON(target); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.JSON(http.StatusRequestEntityTooLarge, validateErrorBody{
				Scope:   "validate",
				Code:    http.StatusRequestEntityTooLarge,
				Message: "Request body too large",
				Errors:  map[string]any{"body": err.Error()},
			})
			return false
		}
		c.JSON(http.StatusNotAcceptable, validateErrorBody{
			Scope:   "validate",
			Code:    http.StatusNotAcceptable,
			Message: "Validation error",
			Errors:  map[string]any{"body": err.Error()},
		})
		return false
	}
	return true
}
