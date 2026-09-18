FROM gcr.io/distroless/static:nonroot
# bin/operator is what scripts/build-fleet.sh's build_go emits (and what
# `just container` now builds to) — one artifact path for both build routes.
COPY bin/operator /schematic-operator
ENTRYPOINT ["/schematic-operator"]
