FROM golang:1.21-alpine AS builder

WORKDIR /app

# install dependencies
COPY go.mod go.sum ./
RUN go mod download

# copy source code
COPY *.go ./
COPY static/ ./static/

# build the application
RUN CGO_ENABLED=0 GOOS=linux go build -o /proton-webdav-bridge

# use a small alpine image for the final image
FROM alpine:latest

# install ca-certificates for HTTPS requests
RUN apk --no-cache add ca-certificates

WORKDIR /app

# create data directory for tokens
RUN mkdir -p /root/.local/share/proton-webdav-bridge

# copy the binary from the builder stage
COPY --from=builder /proton-webdav-bridge /app/proton-webdav-bridge

# environment variables for credentials
# PROTON_USERNAME - your Proton account username
# PROTON_PASSWORD - your Proton account password
# PROTON_MAILBOX_PASSWORD - your mailbox password (optional)
# PROTON_2FA - your 2FA token (optional)

# expose the ports
EXPOSE 7984
EXPOSE 7985

# run the application
ENTRYPOINT ["/app/proton-webdav-bridge"]

# default command (can be overridden)
CMD ["--listen", "0.0.0.0:7984", "--admin-listen", "0.0.0.0:7985"] 