package compose

import (
	"encoding/json"
	"net/http"
	"strings"
)

// RecipesHandler serves the database recipes.
//
// A route rather than a copy in each client, because the CLI cannot import this
// package and the dashboard is not Go at all. Two copies of a recipe is two
// places for the durability decision to drift from what the planner accepts.
func RecipesHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		engine := strings.TrimPrefix(r.PathValue("engine"), "/")
		q := r.URL.Query()

		// pool defaults ON for Postgres: an application that opens a connection
		// per request exhausts a Postgres long before it exhausts the machine,
		// and the operator who has not thought about it is exactly the one who
		// should get the pooler.
		pool := q.Get("pool") != "false" && q.Get("pool") != "0"

		recipe, err := Generate(engine, q.Get("name"), q.Get("mode"), pool)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": err.Error(),
				"code":  "bad_request",
				"next":  "recipes exist for " + strings.Join(Engines, ", "),
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(recipe)
	}
}
