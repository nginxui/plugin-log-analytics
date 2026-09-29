package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/nginxui/plugin-log-analytics/internal/apierr"
	"github.com/nginxui/plugin-log-analytics/internal/service"
)

func writeErrorRecorder(t *testing.T, err error) *httptest.ResponseRecorder {
	t.Helper()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/search", nil)
	writeError(c, err)
	return recorder
}

func TestPlainErrorsAreAnsweredWithAGenericMessage(t *testing.T) {
	recorder := writeErrorRecorder(t, fmt.Errorf("open /var/lib/secret/index: %w", errors.New("permission denied")))

	assert.Equal(t, http.StatusInternalServerError, recorder.Code)
	assert.NotContains(t, recorder.Body.String(), "secret")
	assert.NotContains(t, recorder.Body.String(), "permission denied")

	var body errorBody
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	assert.Equal(t, http.StatusInternalServerError, body.Code)
	assert.Equal(t, "Server Error", body.Message)
}

func TestCodedErrorsKeepTheirShape(t *testing.T) {
	recorder := writeErrorRecorder(t, apierr.WithParams(service.ErrCannotAccessLogFile, "x"))

	assert.Equal(t, http.StatusInternalServerError, recorder.Code)
	var body apierr.Error
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	assert.EqualValues(t, 50015, body.Code)
	assert.Equal(t, "nginx_log", body.Scope)
	assert.Equal(t, []string{"x"}, body.Params)
}

func TestMissingRecordsAre404(t *testing.T) {
	recorder := writeErrorRecorder(t, gorm.ErrRecordNotFound)

	assert.Equal(t, http.StatusNotFound, recorder.Code)
	var body errorBody
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	assert.Equal(t, http.StatusNotFound, body.Code)
}
