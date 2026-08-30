FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/manager ./cmd

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/manager /manager
ENTRYPOINT ["/manager"]
