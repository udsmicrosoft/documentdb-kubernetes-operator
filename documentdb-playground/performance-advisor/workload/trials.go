// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package workload runs the same synthetic trials locally and in operator tests.
package workload

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/documentdb/documentdb-kubernetes-operator/documentdb-playground/performance-advisor/demo"
	"go.mongodb.org/mongo-driver/v2/bson"
	driver "go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const documentCount = 30000

func seed(ctx context.Context, collection *driver.Collection) error {
	const batchSize = 1000
	for start := 0; start < documentCount; start += batchSize {
		docs := make([]any, batchSize)
		for i := range docs {
			docs[i] = bson.D{
				{Key: "_id", Value: start + i}, {Key: "value", Value: start + i},
				{Key: "padding", Value: strings.Repeat("x", 128)},
			}
		}
		result, err := collection.InsertMany(ctx, docs)
		if err != nil {
			return fmt.Errorf("seed synthetic documents: %w", err)
		}
		if len(result.InsertedIDs) != batchSize {
			return fmt.Errorf("seed inserted %d documents, expected %d", len(result.InsertedIDs), batchSize)
		}
	}
	return nil
}

func verifyIndex(ctx context.Context, collection *driver.Collection, expected bool) error {
	cursor, err := collection.Indexes().List(ctx)
	if err != nil {
		return fmt.Errorf("list synthetic indexes: %w", err)
	}
	var indexes []struct {
		Name string `bson:"name"`
	}
	if err := cursor.All(ctx, &indexes); err != nil {
		return fmt.Errorf("read synthetic indexes: %w", err)
	}
	found := false
	for _, index := range indexes {
		found = found || index.Name == "idx_live_value"
	}
	if found != expected {
		return fmt.Errorf("synthetic index state did not match the requested trial")
	}
	return nil
}

func measure(ctx context.Context, deadline time.Time, find func(context.Context) error) (int, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	operations := 0
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return operations, context.Cause(ctx)
		}
		if err := find(ctx); err != nil {
			return operations, err
		}
		operations++
		select {
		case <-ctx.Done():
			return operations, context.Cause(ctx)
		case <-ticker.C:
		}
	}
	return operations, nil
}

// Run seeds a fresh collection and records indexed, unindexed, and restored
// windows. A gate requires live interpretations; ungated integration windows
// retain the existing 45-second duration.
func Run(ctx context.Context, collection *driver.Collection, gate *demo.Coordinator, record func(demo.Trial) error) error {
	if gate != nil {
		ctx = gate.Context()
		if err := gate.WaitReady(); err != nil {
			return err
		}
	}
	if err := seed(ctx, collection); err != nil {
		return err
	}
	index := driver.IndexModel{
		Keys: bson.D{{Key: "value", Value: 1}}, Options: options.Index().SetName("idx_live_value"),
	}
	for trial, indexed := range []bool{true, false, true} {
		if indexed {
			if _, err := collection.Indexes().CreateOne(ctx, index); err != nil {
				return fmt.Errorf("create synthetic index: %w", err)
			}
		} else if err := collection.Indexes().DropOne(ctx, "idx_live_value"); err != nil {
			return fmt.Errorf("drop synthetic index: %w", err)
		}
		if err := verifyIndex(ctx, collection, indexed); err != nil {
			return err
		}
		id := fmt.Sprintf("trial-%d", trial+1)
		// Use the same UTC deadline as the cross-process report gate.
		start := time.Now().UTC()
		duration := 45 * time.Second
		if gate != nil {
			duration = demo.TrialDuration
			if err := gate.BeginTrial(id, start); err != nil {
				return err
			}
		}
		deadline := start.Add(duration)
		operations, err := measure(ctx, deadline, func(ctx context.Context) error {
			var result struct {
				ID    int `bson:"_id"`
				Value int `bson:"value"`
			}
			if err := collection.FindOne(ctx, bson.D{{Key: "value", Value: documentCount - 1}}).Decode(&result); err != nil {
				return fmt.Errorf("synthetic lookup: %w", err)
			}
			if result.ID != documentCount-1 || result.Value != documentCount-1 {
				return fmt.Errorf("synthetic lookup returned an unexpected document")
			}
			return nil
		})
		if err != nil {
			return err
		}
		end := time.Now().UTC()
		if gate != nil {
			if err := gate.EndTrial(end, operations); err != nil {
				return err
			}
		}
		if err := record(demo.Trial{ID: id, Start: start, Deadline: deadline, End: end, Operations: operations}); err != nil {
			return err
		}
	}
	return nil
}
