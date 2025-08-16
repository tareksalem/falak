package main

import (
	"fmt"
	"github.com/tareksalem/falak/internal/node"
)

func main() {
	// Example 1: Basic cluster-based topic
	topic1 := node.Topic{
		Name: "membership",
		Type: node.TopicTypeGossip,
		Path: "falak/{cluster}/{name}",
	}
	topic1.SetParam("cluster", "production")
	fmt.Printf("Topic 1: %s\n", topic1.GetFullName())

	// Example 2: Simple topic without cluster
	topic2 := node.Topic{
		Name: "alerts",
		Type: node.TopicTypeFlood,
		Path: "{name}",
	}
	fmt.Printf("Topic 2: %s\n", topic2.GetFullName())

	// Example 3: Complex API-style path
	topic3 := node.Topic{
		Name: "user-events",
		Type: node.TopicTypeGossip,
		Path: "api/{version}/users/{userId}/events/{name}",
	}
	topic3.SetParam("version", "v2")
	topic3.SetParam("userId", "user-123")
	fmt.Printf("Topic 3: %s\n", topic3.GetFullName())

	// Example 4: Regional data synchronization
	topic4 := node.Topic{
		Name: "sync",
		Type: node.TopicTypeFlood,
		Path: "{datacenter}/{region}/{environment}/{name}",
	}
	topic4.SetParam("datacenter", "us-west")
	topic4.SetParam("region", "california")
	topic4.SetParam("environment", "staging")
	fmt.Printf("Topic 4: %s\n", topic4.GetFullName())

	// Example 5: Global announcements
	topic5 := node.Topic{
		Name: "system-maintenance",
		Type: node.TopicTypeFlood,
		Path: "global/announcements/{name}",
	}
	fmt.Printf("Topic 5: %s\n", topic5.GetFullName())

	fmt.Printf("\n=== Default cluster topics ===\n")
	defaultTopics := node.GetDefaultTopics("production")
	for _, topic := range defaultTopics {
		fmt.Printf("- %s (%s): %s\n", topic.Name, topic.Type, topic.GetFullName())
	}
}