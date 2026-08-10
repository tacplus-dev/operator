FROM gcr.io/distroless/static:nonroot
COPY schematic-operator /schematic-operator
ENTRYPOINT ["/schematic-operator"]
