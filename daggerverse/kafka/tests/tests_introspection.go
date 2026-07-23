package main

import (
	"context"
	"encoding/json"
	"fmt"

	"dagger/tests/internal/dagger"
)

// topicDescription / groupDescription mirror the JSON shapes the kafka
// module's DescribeTopic and DescribeConsumerGroup write to their returned
// *dagger.File. The tests export the file's contents and unmarshal into these
// local types.
type topicDescription struct {
	Name              string             `json:"name"`
	PartitionCount    int                `json:"partitionCount"`
	ReplicationFactor int                `json:"replicationFactor"`
	Partitions        []topicPartition   `json:"partitions"`
	Configs           []topicConfigEntry `json:"configs"`
}

type topicPartition struct {
	Partition int32   `json:"partition"`
	Leader    int32   `json:"leader"`
	Replicas  []int32 `json:"replicas"`
	Isr       []int32 `json:"isr"`
}

type topicConfigEntry struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Source string `json:"source"`
}

type groupDescription struct {
	Group        string         `json:"group"`
	State        string         `json:"state"`
	ProtocolType string         `json:"protocolType"`
	Protocol     string         `json:"protocol"`
	Coordinator  int32          `json:"coordinator"`
	Members      []groupMember  `json:"members"`
	Lag          []partitionLag `json:"lag"`
	TotalLag     int64          `json:"totalLag"`
}

type groupMember struct {
	MemberID   string             `json:"memberId"`
	ClientID   string             `json:"clientId"`
	ClientHost string             `json:"clientHost"`
	InstanceID string             `json:"instanceId,omitempty"`
	Assigned   []memberAssignment `json:"assigned"`
}

type memberAssignment struct {
	Topic      string  `json:"topic"`
	Partitions []int32 `json:"partitions"`
}

type partitionLag struct {
	Topic     string `json:"topic"`
	Partition int32  `json:"partition"`
	Commit    int64  `json:"commit"`
	End       int64  `json:"end"`
	Lag       int64  `json:"lag"`
	Member    string `json:"member,omitempty"`
}

func describeTopic(ctx context.Context, client *dagger.KafkaClient, topic string) (topicDescription, error) {
	raw, err := client.DescribeTopic(topic).Contents(ctx)
	if err != nil {
		return topicDescription{}, fmt.Errorf("describe topic %q: %w", topic, err)
	}
	var td topicDescription
	if err := json.Unmarshal([]byte(raw), &td); err != nil {
		return topicDescription{}, fmt.Errorf("unmarshal topic description: %w", err)
	}
	return td, nil
}

func describeConsumerGroup(ctx context.Context, client *dagger.KafkaClient, group string) (groupDescription, error) {
	raw, err := client.DescribeConsumerGroup(group).Contents(ctx)
	if err != nil {
		return groupDescription{}, fmt.Errorf("describe consumer group %q: %w", group, err)
	}
	var gd groupDescription
	if err := json.Unmarshal([]byte(raw), &gd); err != nil {
		return groupDescription{}, fmt.Errorf("unmarshal group description: %w", err)
	}
	return gd, nil
}

// DescribeTopicReportsPartitionsAndConfigs creates a 3-partition RF=1 topic
// and asserts DescribeTopic reports the derived partition count / replication
// factor, one partition entry per partition, and a non-empty topic-level
// config set (proving the configs path is wired).
func (t *Tests) DescribeTopicReportsPartitionsAndConfigs(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, err := freshCluster(ctx, kafkaImageTag)
	if err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}
	defer cluster.Stop(ctx)
	return describeTopicReportsPartitionsAndConfigsOn(ctx, cluster)
}

func describeTopicReportsPartitionsAndConfigsOn(ctx context.Context, cluster *dagger.KafkaCluster) error {
	client := cluster.Client(dag.Kafka().PlaintextClientSecurity())

	topic, err := randomTopicName(ctx)
	if err != nil {
		return err
	}
	if err := client.CreateTopic(ctx, topic, dagger.KafkaClientCreateTopicOpts{
		Partitions:        3,
		ReplicationFactor: 1,
	}); err != nil {
		return fmt.Errorf("create topic %q: %w", topic, err)
	}

	td, err := describeTopic(ctx, client, topic)
	if err != nil {
		return err
	}
	if td.Name != topic {
		return fmt.Errorf("name mismatch: want %q, got %q", topic, td.Name)
	}
	if td.PartitionCount != 3 {
		return fmt.Errorf("partitionCount mismatch: want 3, got %d", td.PartitionCount)
	}
	if td.ReplicationFactor != 1 {
		return fmt.Errorf("replicationFactor mismatch: want 1, got %d", td.ReplicationFactor)
	}
	if len(td.Partitions) != 3 {
		return fmt.Errorf("expected 3 partition entries, got %d", len(td.Partitions))
	}
	for _, p := range td.Partitions {
		if len(p.Replicas) != 1 {
			return fmt.Errorf("partition %d: expected 1 replica, got %v", p.Partition, p.Replicas)
		}
	}
	// A topic always carries broker-defaulted configs (cleanup.policy,
	// retention.ms, ...); an empty set means the configs path never ran.
	if len(td.Configs) == 0 {
		return fmt.Errorf("expected non-empty topic configs, got none")
	}
	if !hasConfigKey(td.Configs, "cleanup.policy") {
		return fmt.Errorf("expected a cleanup.policy config entry, got %v", configKeys(td.Configs))
	}
	return nil
}

// ListConsumerGroupsReportsCommittedGroup produces a record, consumes it back
// through a committing consumer group, and asserts the group then appears in
// ListConsumerGroups. A fresh cluster reports no groups, so the group's
// presence proves the join + commit reached __consumer_offsets.
func (t *Tests) ListConsumerGroupsReportsCommittedGroup(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, err := freshCluster(ctx, kafkaImageTag)
	if err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}
	defer cluster.Stop(ctx)
	return listConsumerGroupsReportsCommittedGroupOn(ctx, cluster)
}

func listConsumerGroupsReportsCommittedGroupOn(ctx context.Context, cluster *dagger.KafkaCluster) error {
	client := cluster.Client(dag.Kafka().PlaintextClientSecurity())

	topic, err := randomTopicName(ctx)
	if err != nil {
		return err
	}
	if err := client.CreateTopic(ctx, topic, dagger.KafkaClientCreateTopicOpts{
		Partitions:        1,
		ReplicationFactor: 1,
	}); err != nil {
		return fmt.Errorf("create topic %q: %w", topic, err)
	}
	if err := client.Produce(ctx, topic, "k", "v", dagger.KafkaClientProduceOpts{
		KeyEncoding:   "raw",
		ValueEncoding: "raw",
	}); err != nil {
		return fmt.Errorf("produce: %w", err)
	}

	group, err := randomTopicName(ctx)
	if err != nil {
		return err
	}
	if _, err := consume(ctx, client, topic, dagger.KafkaClientConsumeOpts{
		MaxMessages:   1,
		Timeout:       "20s",
		KeyEncoding:   "raw",
		ValueEncoding: "raw",
		Group:         group,
		CommitOffsets: true,
	}); err != nil {
		return fmt.Errorf("consume with committing group: %w", err)
	}

	groups, err := client.ListConsumerGroups(ctx)
	if err != nil {
		return fmt.Errorf("list consumer groups: %w", err)
	}
	if !contains(groups, group) {
		return fmt.Errorf("expected group %q in %v", group, groups)
	}
	return nil
}

// DescribeConsumerGroupReportsLag produces five records, consumes three of
// them through a committing consumer group, and asserts DescribeConsumerGroup
// reports the group in the Empty state with committed-offset lag of 2 (end
// offset 5 minus committed offset 3) on the single partition.
func (t *Tests) DescribeConsumerGroupReportsLag(
	ctx context.Context,
	// +default="4.2.0"
	kafkaImageTag string,
) error {
	cluster, err := freshCluster(ctx, kafkaImageTag)
	if err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}
	defer cluster.Stop(ctx)
	return describeConsumerGroupReportsLagOn(ctx, cluster)
}

func describeConsumerGroupReportsLagOn(ctx context.Context, cluster *dagger.KafkaCluster) error {
	client := cluster.Client(dag.Kafka().PlaintextClientSecurity())

	topic, err := randomTopicName(ctx)
	if err != nil {
		return err
	}
	if err := client.CreateTopic(ctx, topic, dagger.KafkaClientCreateTopicOpts{
		Partitions:        1,
		ReplicationFactor: 1,
	}); err != nil {
		return fmt.Errorf("create topic %q: %w", topic, err)
	}

	const produced = 5
	for i := range produced {
		if err := client.Produce(ctx, topic, "k", fmt.Sprintf("v%d", i), dagger.KafkaClientProduceOpts{
			KeyEncoding:   "raw",
			ValueEncoding: "raw",
		}); err != nil {
			return fmt.Errorf("produce record %d: %w", i, err)
		}
	}

	group, err := randomTopicName(ctx)
	if err != nil {
		return err
	}
	const consumed = 3
	records, err := consume(ctx, client, topic, dagger.KafkaClientConsumeOpts{
		MaxMessages:   consumed,
		Timeout:       "20s",
		KeyEncoding:   "raw",
		ValueEncoding: "raw",
		Group:         group,
		CommitOffsets: true,
	})
	if err != nil {
		return fmt.Errorf("consume with committing group: %w", err)
	}
	if len(records) != consumed {
		return fmt.Errorf("expected %d records consumed, got %d", consumed, len(records))
	}

	gd, err := describeConsumerGroup(ctx, client, group)
	if err != nil {
		return err
	}
	if gd.Group != group {
		return fmt.Errorf("group name mismatch: want %q, got %q", group, gd.Group)
	}
	// All members have left after the consume returned, so the group is Empty
	// but retains its committed offsets.
	if gd.State != "Empty" {
		return fmt.Errorf("expected Empty group state, got %q", gd.State)
	}
	if gd.TotalLag != produced-consumed {
		return fmt.Errorf("total lag mismatch: want %d, got %d", produced-consumed, gd.TotalLag)
	}
	if len(gd.Lag) != 1 {
		return fmt.Errorf("expected lag for 1 partition, got %d entries: %+v", len(gd.Lag), gd.Lag)
	}
	pl := gd.Lag[0]
	if pl.Topic != topic || pl.Partition != 0 {
		return fmt.Errorf("lag entry targets wrong partition: %+v", pl)
	}
	if pl.Commit != consumed {
		return fmt.Errorf("committed offset mismatch: want %d, got %d", consumed, pl.Commit)
	}
	if pl.End != produced {
		return fmt.Errorf("end offset mismatch: want %d, got %d", produced, pl.End)
	}
	if pl.Lag != produced-consumed {
		return fmt.Errorf("partition lag mismatch: want %d, got %d", produced-consumed, pl.Lag)
	}
	return nil
}

func hasConfigKey(configs []topicConfigEntry, key string) bool {
	for _, c := range configs {
		if c.Key == key {
			return true
		}
	}
	return false
}

func configKeys(configs []topicConfigEntry) []string {
	keys := make([]string, 0, len(configs))
	for _, c := range configs {
		keys = append(keys, c.Key)
	}
	return keys
}
