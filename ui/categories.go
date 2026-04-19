package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"sync"
)

const categoriesPath = "/config/categories.json"

// Category groups containers under a named label.
type Category struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Color       string   `json:"color,omitempty"`  // border color hex (e.g. "#58a6ff")
	Containers  []string `json:"containers"`       // container names
	Order       int      `json:"order"`             // display order (lower = first)
}

type categoryStore struct {
	mu         sync.RWMutex
	categories []Category
}

func newCategoryStore() *categoryStore {
	cs := &categoryStore{}
	cs.load()
	return cs
}

func (cs *categoryStore) load() {
	data, err := os.ReadFile(categoriesPath)
	if err != nil {
		return // no file yet — empty categories
	}
	var cats []Category
	if err := json.Unmarshal(data, &cats); err != nil {
		log.Printf("Warning: failed to parse categories.json: %v", err)
		return
	}
	cs.categories = cats
}

func (cs *categoryStore) save() error {
	data, err := json.MarshalIndent(cs.categories, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWriteFile(categoriesPath, data, 0664); err != nil {
		return err
	}
	// Match ownership with other config files (nobody:users = 99:100)
	_ = os.Chown(categoriesPath, 99, 100)
	return nil
}

func (cs *categoryStore) List() []Category {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	result := make([]Category, len(cs.categories))
	copy(result, cs.categories)
	sort.Slice(result, func(i, j int) bool {
		return result[i].Order < result[j].Order
	})
	return result
}

func (cs *categoryStore) Update(categories []Category) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	old := cs.categories
	cs.categories = categories
	if err := cs.save(); err != nil {
		cs.categories = old // rollback on save failure
		return err
	}
	return nil
}

// validateCategories checks category data for obvious problems.
func validateCategories(cats []Category) error {
	if len(cats) > 50 {
		return fmt.Errorf("too many categories (max 50)")
	}
	for i, cat := range cats {
		if cat.Name == "" {
			return fmt.Errorf("category %d: name is required", i+1)
		}
		if len(cat.Name) > 100 {
			return fmt.Errorf("category %d: name too long (max 100)", i+1)
		}
		if len(cat.Description) > 500 {
			return fmt.Errorf("category %d: description too long (max 500)", i+1)
		}
		if len(cat.Containers) > 200 {
			return fmt.Errorf("category %d: too many containers (max 200)", i+1)
		}
	}
	return nil
}

// handleListCategories returns all categories.
func (cs *categoryStore) handleListCategories(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cs.List())
}

// handleUpdateCategories replaces all categories.
func (cs *categoryStore) handleUpdateCategories(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	var cats []Category
	if err := json.NewDecoder(r.Body).Decode(&cats); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, 400)
		return
	}
	if err := validateCategories(cats); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), 400)
		return
	}
	if err := cs.Update(cats); err != nil {
		http.Error(w, `{"error":"failed to save"}`, 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cats)
}
