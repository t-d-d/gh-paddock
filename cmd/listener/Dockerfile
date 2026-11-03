# The listener's image: a static Go binary on distroless, built native arm64
# by the deploy (.github/actions/image). The build context is the repo root;
# .dockerignore keeps it to the Go module. The deploy's image tag hashes the
# files this copies, and test/deploy-workflow.test.ts holds the two lists
# together.
FROM public.ecr.aws/docker/library/golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/listener ./cmd/listener

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/listener /listener
USER nonroot
ENTRYPOINT ["/listener"]
