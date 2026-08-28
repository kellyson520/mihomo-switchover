FROM golang:1.24-alpine AS source
WORKDIR /src
COPY go.mod go.sum ./
COPY . .

FROM source AS test
RUN go test -mod=vendor ./... && go vet -mod=vendor ./...

FROM source AS binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=vendor -trimpath -ldflags='-s -w' -o /out/guardian ./cmd/guardian
