package api

import (
	"net/http"

	"github.com/formancehq/orchestration/internal/workflow"
	"github.com/formancehq/stack/libs/go-libs/api"
)

func abortWorkflowInstance(m *workflow.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := m.AbortRun(r.Context(), instanceID(r)); err != nil {
			api.InternalServerError(w, r, err)
			return
		}
		api.NoContent(w)
	}
}
