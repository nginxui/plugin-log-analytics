package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/nginxui/plugin-log-analytics/internal/config"
)

var geoBoundaryFileNamePattern = regexp.MustCompile(`^\d{6}_full\.json$`)

// GetGeoBoundaryFile serves one map boundary file from the configured map
// directory. The page falls back to a CDN when the file is not there.
func GetGeoBoundaryFile(c *gin.Context) {
	filename := strings.TrimSpace(c.Param("filename"))
	if !geoBoundaryFileNamePattern.MatchString(filename) {
		c.JSON(http.StatusBadRequest, gin.H{
			"message": "invalid map file name",
		})
		return
	}

	filePath := filepath.Join(config.GeoMapDir(), filename)
	fileBytes, err := os.ReadFile(filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.JSON(http.StatusNotFound, gin.H{
				"message": "map file not found",
			})
			return
		}

		c.JSON(http.StatusInternalServerError, gin.H{
			"message": "failed to read map file",
		})
		return
	}

	c.Data(http.StatusOK, "application/json; charset=utf-8", fileBytes)
}
