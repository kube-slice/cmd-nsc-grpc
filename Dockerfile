# Build stage
FROM golang:1.24 AS build
WORKDIR /build

# Download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build a fully static binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /bin/app .

# Runtime stage
FROM alpine:3.20.1

# Copy binary from build stage
COPY --from=build /bin/app /bin/app

# Set entrypoint
ENTRYPOINT ["/bin/app"]
