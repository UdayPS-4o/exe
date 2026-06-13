FROM golang:1.23-alpine

# Install build/git dependencies
RUN apk add --no-cache git

# Install goversioninfo utility
RUN go install github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest

# Set workspace
WORKDIR /app

# Pre-download dependencies (cache layer)
COPY go.mod ./
# Copy go.sum if present to verify dependencies
COPY go.sum* ./
RUN go mod download

# Copy application sources
COPY . .

# Build the HTTP compilation server
RUN CGO_ENABLED=0 GOOS=linux go build -o /pdf-to-exe-server .

# Default service port
EXPOSE 8080

# Run service
CMD ["/pdf-to-exe-server"]
