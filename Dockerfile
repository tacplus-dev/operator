FROM gcr.io/distroless/static:nonroot
COPY cavalier-operator /cavalier-operator
ENTRYPOINT ["/cavalier-operator"]
