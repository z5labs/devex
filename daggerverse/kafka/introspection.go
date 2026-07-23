package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	"dagger/kafka/internal/dagger"

	"github.com/twmb/franz-go/pkg/kadm"
)

// topicDescription is the JSON shape DescribeTopic writes to its returned
// *dagger.File: per-partition leader/replica/ISR layout, the derived partition
// count and replication factor, and the full topic-level config set (so a
// downstream skill generator can render retention, cleanup policy, and the
// rest deterministically). Configs are sorted by key so the output is stable
// across calls.
type topicDescription struct {
	Name              string             `json:"name"`
	PartitionCount    int                `json:"partitionCount"`
	ReplicationFactor int                `json:"replicationFactor"`
	Partitions        []topicPartition   `json:"partitions"`
	Configs           []topicConfigEntry `json:"configs"`
}

// topicPartition is one partition's replica layout as reported by the broker
// metadata: the elected leader broker, the assigned replica set, and the
// in-sync replica subset.
type topicPartition struct {
	Partition int32   `json:"partition"`
	Leader    int32   `json:"leader"`
	Replicas  []int32 `json:"replicas"`
	Isr       []int32 `json:"isr"`
}

// topicConfigEntry is a single topic configuration key/value plus the source
// the broker attributes it to (e.g. DYNAMIC_TOPIC_CONFIG for an explicitly-set
// override vs DEFAULT_CONFIG for a broker default). Sensitive configs report
// an empty value.
type topicConfigEntry struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Source string `json:"source"`
}

// groupDescription is the JSON shape DescribeConsumerGroup writes: the group's
// coordinator/state/protocol, its live members with their partition
// assignments, and the per-partition committed-offset lag (plus the total).
// Lag entries are only populated for partitions the group has committed
// offsets for; an Empty group with committed offsets still reports lag against
// the current end offset.
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

// groupMember is one member of a consumer group, with the partitions the
// leader assigned it grouped by topic.
type groupMember struct {
	MemberID   string             `json:"memberId"`
	ClientID   string             `json:"clientId"`
	ClientHost string             `json:"clientHost"`
	InstanceID string             `json:"instanceId,omitempty"`
	Assigned   []memberAssignment `json:"assigned"`
}

// memberAssignment is the set of partitions of a single topic assigned to a
// group member.
type memberAssignment struct {
	Topic      string  `json:"topic"`
	Partitions []int32 `json:"partitions"`
}

// partitionLag is the committed-offset lag for one topic partition within a
// group: the group's committed offset, the partition's current end offset, and
// the difference. Member is the member id currently assigned the partition, or
// empty when the group is Empty.
type partitionLag struct {
	Topic     string `json:"topic"`
	Partition int32  `json:"partition"`
	Commit    int64  `json:"commit"`
	End       int64  `json:"end"`
	Lag       int64  `json:"lag"`
	Member    string `json:"member,omitempty"`
}

// DescribeTopic returns per-topic metadata as JSON: the partition layout
// (leader, replicas, ISR per partition), the derived partition count and
// replication factor, and the topic-level configuration set (retention,
// cleanup policy, and so on). The JSON is returned as a *dagger.File so it
// crosses the module boundary as a core type; callers export it and unmarshal
// the bytes themselves.
//
// +cache="never"
func (c *Client) DescribeTopic(ctx context.Context, name string) (*dagger.File, error) {
	cl, err := c.newKgoClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("new kafka client: %w", err)
	}
	defer cl.Close()

	adm := kadm.NewClient(cl)

	details, err := adm.ListTopics(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("describe topic %q: %w", name, err)
	}
	detail, ok := details[name]
	if !ok {
		return nil, fmt.Errorf("topic %q not found", name)
	}
	if detail.Err != nil {
		return nil, fmt.Errorf("describe topic %q: %w", name, detail.Err)
	}

	desc := topicDescription{
		Name:              name,
		PartitionCount:    len(detail.Partitions),
		ReplicationFactor: detail.Partitions.NumReplicas(),
	}
	for _, p := range detail.Partitions.Sorted() {
		desc.Partitions = append(desc.Partitions, topicPartition{
			Partition: p.Partition,
			Leader:    p.Leader,
			Replicas:  p.Replicas,
			Isr:       p.ISR,
		})
	}

	configs, err := adm.DescribeTopicConfigs(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("describe configs for topic %q: %w", name, err)
	}
	rc, err := configs.On(name, nil)
	if err != nil {
		return nil, fmt.Errorf("configs for topic %q: %w", name, err)
	}
	if rc.Err != nil {
		return nil, fmt.Errorf("configs for topic %q: %w", name, rc.Err)
	}
	for _, cfg := range rc.Configs {
		desc.Configs = append(desc.Configs, topicConfigEntry{
			Key:    cfg.Key,
			Value:  cfg.MaybeValue(),
			Source: cfg.Source.String(),
		})
	}
	sort.Slice(desc.Configs, func(i, j int) bool {
		return desc.Configs[i].Key < desc.Configs[j].Key
	})

	return marshalToWorkdirFile("topic", "topic.json", desc)
}

// ListConsumerGroups returns the names of every consumer group the cluster
// reports, sorted. A fresh cluster reports none; a group appears once a
// consumer has joined it and persists (in the Empty state) while it retains
// committed offsets.
//
// +cache="never"
func (c *Client) ListConsumerGroups(ctx context.Context) ([]string, error) {
	cl, err := c.newKgoClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("new kafka client: %w", err)
	}
	defer cl.Close()

	adm := kadm.NewClient(cl)
	groups, err := adm.ListGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("list consumer groups: %w", err)
	}
	return groups.Groups(), nil
}

// DescribeConsumerGroup returns a consumer group's detail as JSON: its
// coordinator, state, and assignment protocol; its live members with the
// partitions assigned to each; and the per-partition committed-offset lag
// (with the total). Lag is only reported for partitions the group has
// committed offsets for. The JSON is returned as a *dagger.File so it crosses
// the module boundary as a core type.
//
// +cache="never"
func (c *Client) DescribeConsumerGroup(ctx context.Context, group string) (*dagger.File, error) {
	cl, err := c.newKgoClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("new kafka client: %w", err)
	}
	defer cl.Close()

	adm := kadm.NewClient(cl)
	lags, err := adm.Lag(ctx, group)
	if err != nil {
		return nil, fmt.Errorf("describe consumer group %q: %w", group, err)
	}
	dl, ok := lags[group]
	if !ok {
		return nil, fmt.Errorf("consumer group %q not found", group)
	}
	if err := dl.Error(); err != nil {
		return nil, fmt.Errorf("describe consumer group %q: %w", group, err)
	}

	desc := groupDescription{
		Group:        dl.Group,
		State:        dl.State,
		ProtocolType: dl.ProtocolType,
		Protocol:     dl.Protocol,
		Coordinator:  dl.Coordinator.NodeID,
	}
	for _, m := range dl.Members {
		gm := groupMember{
			MemberID:   m.MemberID,
			ClientID:   m.ClientID,
			ClientHost: m.ClientHost,
		}
		if m.InstanceID != nil {
			gm.InstanceID = *m.InstanceID
		}
		if ca, ok := m.Assigned.AsConsumer(); ok {
			for _, t := range ca.Topics {
				parts := append([]int32(nil), t.Partitions...)
				slices.Sort(parts)
				gm.Assigned = append(gm.Assigned, memberAssignment{
					Topic:      t.Topic,
					Partitions: parts,
				})
			}
			sort.Slice(gm.Assigned, func(i, j int) bool {
				return gm.Assigned[i].Topic < gm.Assigned[j].Topic
			})
		}
		desc.Members = append(desc.Members, gm)
	}
	for _, gl := range dl.Lag.Sorted() {
		pl := partitionLag{
			Topic:     gl.Topic,
			Partition: gl.Partition,
			Commit:    gl.Commit.At,
			End:       gl.End.Offset,
			Lag:       gl.Lag,
		}
		if gl.Member != nil {
			pl.Member = gl.Member.MemberID
		}
		desc.Lag = append(desc.Lag, pl)
	}
	desc.TotalLag = dl.Lag.Total()

	return marshalToWorkdirFile("group", "group.json", desc)
}

// marshalToWorkdirFile encodes v as indented JSON and writes it into the
// module runtime's scratch workdir as a *dagger.File, reusing the
// content-addressed writeWorkdirBytes helper so identical descriptions
// collapse to one file.
func marshalToWorkdirFile(label, name string, v any) (*dagger.File, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal %s json: %w", label, err)
	}
	return writeWorkdirBytes(label, name, b)
}
