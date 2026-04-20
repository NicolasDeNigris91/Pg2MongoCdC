# Extends Debezium's Kafka Connect image with the MongoDB Kafka Connector.
#
# NOTE on Avro converter: Week 1 uses JsonConverter (built into Kafka Connect,
# zero extra deps) to keep the walking skeleton fast to boot. Avro +
# Confluent Schema Registry are documented as the target wire format in
# ADR-006 and will land when we control the sink ourselves in Week 2.

FROM debezium/connect:2.6

ARG MONGO_CONNECTOR_VERSION=1.13.0

USER root

RUN mkdir -p /kafka/connect/mongodb-kafka-connect \
 && curl -fsSL \
      "https://repo1.maven.org/maven2/org/mongodb/kafka/mongo-kafka-connect/${MONGO_CONNECTOR_VERSION}/mongo-kafka-connect-${MONGO_CONNECTOR_VERSION}-all.jar" \
      -o /kafka/connect/mongodb-kafka-connect/mongo-kafka-connect.jar \
 && chown -R kafka:kafka /kafka/connect/mongodb-kafka-connect

USER kafka
