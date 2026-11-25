FROM gcr.io/distroless/static:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5
# bin/operator is what scripts/build-fleet.sh's build_go emits (and what
# `just container` now builds to) — one artifact path for both build routes.
COPY bin/operator /schematic-operator
# The base's nonroot user, said here so the image stays non-root whatever the
# base (#26). The operator writes nothing in the image: /config is mounted.
USER 65532:65532
ENTRYPOINT ["/schematic-operator"]
