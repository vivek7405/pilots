package compose

import (
	"encoding/json"
	"net/http"
	"strconv"
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

// HAFragmentHandler serves the conversion for one database.
//
// Served rather than built in the CLI for the reason every recipe is: the
// generator lives beside the planner that has to accept its output, and a
// second copy in a client would drift from it silently.
func HAFragmentHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		replicas := atoiOr(q.Get("replicas"), 2)
		etcd := atoiOr(q.Get("etcd"), 3)

		frag, err := HAFragmentFor(strings.TrimPrefix(r.PathValue("name"), "/"), replicas, etcd)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": err.Error(),
				"code":  "bad_request",
				"next":  "2 to 7 data replicas, and an odd number of etcd members from 3 to 9",
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(frag)
	}
}

// atoiOr is a query number with a default, because a missing count and an
// unreadable one both mean "the caller did not choose", and the recipe's own
// default is a better answer than a 400 about a parameter nobody typed.
func atoiOr(raw string, fallback int) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}
