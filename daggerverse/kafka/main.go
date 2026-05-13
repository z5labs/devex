// Kafka provides Dagger functions for spinning up KRaft Kafka clusters from
// one of four upstream images — apache/kafka-native (GraalVM), apache/kafka
// (JVM), confluentinc/cp-kafka (Confluent Platform), or
// redpandadata/redpanda (Redpanda) — and a pure-Go franz-go client that
// targets either the local cluster or any reachable remote cluster.
//
// File map (all `package main`, surfaced as one Dagger module):
//
//   - security.go        — *ServerSecurity / *ClientSecurity + the six
//                          Plaintext/Tls/Mtls constructors.
//   - cluster_kafka.go   — *Cluster + the three KAFKA_*-env-var-contract
//                          distros (ApacheNativeCluster, ApacheCluster,
//                          ConfluentCluster) + buildKafkaCluster.
//   - internal_ca.go     — per-cluster internal mTLS material, caller-CA
//                          external leaf signing, and the Kafka SSL env
//                          var helpers that mount them onto a broker
//                          container.
//   - cluster_redpanda.go — *RedpandaCluster / *RedpandaServerSecurity,
//                          single-node-only Redpanda constructor, rpk
//                          start args, and the redpanda.yaml renderer.
//   - client.go          — *Client + ConsumedRecord, franz-go wiring,
//                          PKCS#12 → *tls.Config, PropertiesFile, and the
//                          admin / produce / consume / list-topics
//                          method set.
//   - util.go            — shared helpers (writeWorkdirBytes,
//                          clusterHostSuffix, randSuffix, dagFileBytes).
package main

// Kafka is the root namespace for every exported function in this module.
// All cluster constructors and security helpers hang off *Kafka so the
// generated Dagger SDK surfaces them under `dag.Kafka().<Func>(...)`.
type Kafka struct{}
