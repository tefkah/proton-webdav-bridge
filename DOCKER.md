# Docker Usage

This project provides a Docker image that can be used to run the Proton WebDAV Bridge without installing it locally.

## Using the pre-built image

The Docker image is automatically built and published to GitHub Container Registry. You can pull it with:

```bash
docker pull ghcr.io/stolld/proton-webdav-bridge:latest
```

## Running the container

### Authentication Options

There are three ways to authenticate with Proton Drive:

1. **Web UI** - Login through the web admin interface (recommended)
2. **Interactive login** - Logging in manually with prompts
3. **Environment variables** - Providing credentials via environment variables

#### Option 1: Web UI (Recommended)

Run the container without any login credentials:

```bash
docker run -d \
  --name proton-webdav \
  -p 7984:7984 \
  -p 7985:7985 \
  -v proton-webdav-data:/root/.local/share \
  ghcr.io/stolld/proton-webdav-bridge:latest
```

Then open `http://localhost:7985` in your browser to access the admin interface. You'll be prompted to log in with your Proton credentials. After successful login, the WebDAV server will start automatically.

This approach is particularly useful for:

- Systems using 2FA authentication
- When tokens expire and need to be refreshed
- When you prefer not to store credentials in environment variables or scripts

#### Option 2: Interactive Login

To login to your Proton account interactively, run:

```bash
docker run -it --rm \
  -v proton-webdav-data:/root/.local/share \
  ghcr.io/stolld/proton-webdav-bridge:latest --login
```

This will prompt you for your credentials and store the authentication tokens in the mounted volume.

#### Option 3: Environment Variables

To run in a non-interactive environment, you can provide your credentials via environment variables:

```bash
docker run -d \
  --name proton-webdav \
  -p 7984:7984 \
  -p 7985:7985 \
  -v proton-webdav-data:/root/.local/share \
  -e PROTON_USERNAME=your-username \
  -e PROTON_PASSWORD=your-password \
  -e PROTON_MAILBOX_PASSWORD=your-mailbox-password \
  -e PROTON_2FA=your-2fa-token \
  ghcr.io/stolld/proton-webdav-bridge:latest
```

Required environment variables:

- `PROTON_USERNAME`: Your Proton account username
- `PROTON_PASSWORD`: Your Proton account password

Optional environment variables:

- `PROTON_MAILBOX_PASSWORD`: Your mailbox password (if you have one)
- `PROTON_2FA`: Your 2FA token (if 2FA is enabled)

Both optional variables can be explicitly set to `"false"` to indicate they should be skipped:

```bash
-e PROTON_MAILBOX_PASSWORD=false
-e PROTON_2FA=false
```

**Note**: With environment variables set, the application will automatically login and regenerate tokens when they expire, making it suitable for server deployments.

### Running the bridge

After setting up authentication, you can run the WebDAV bridge with:

```bash
docker run -d \
  --name proton-webdav \
  -p 7984:7984 \
  -p 7985:7985 \
  -v proton-webdav-data:/root/.local/share \
  ghcr.io/stolld/proton-webdav-bridge:latest
```

This will:

- Run the container in detached mode (`-d`)
- Name the container `proton-webdav`
- Map WebDAV port 7984 from the container to your host
- Map admin interface port 7985 from the container to your host
- Mount the volume with your authentication tokens

The WebDAV server will be accessible at `http://localhost:7984`.  
The admin interface will be accessible at `http://localhost:7985`.

### Using with docker-compose

For more permanent setups, you can use docker-compose:

```yaml
version: "3"

services:
  proton-webdav:
    image: ghcr.io/stolld/proton-webdav-bridge:latest
    container_name: proton-webdav
    restart: unless-stopped
    ports:
      - "7984:7984"
      - "7985:7985"
    volumes:
      - proton-webdav-data:/root/.local/share
    environment:
      - PROTON_USERNAME=your-username
      - PROTON_PASSWORD=your-password
      # Optional variables
      - PROTON_MAILBOX_PASSWORD=false # Skip mailbox password
      - PROTON_2FA=false # Skip 2FA token

volumes:
  proton-webdav-data:
```

### Securing your credentials

For production use, consider using Docker secrets or encrypted environment files to store your credentials securely.

## Admin Interface

The bridge now includes a web-based admin interface that allows you to:

1. View the current connection status
2. Login with your Proton credentials when tokens expire
3. Logout (delete saved tokens)

This is particularly useful when using 2FA, as it provides a web form for entering your credentials including 2FA token when needed. The admin interface is accessible at `http://localhost:7985` by default.

### No Valid Tokens

When starting without valid tokens, the bridge will:

1. Start the admin interface on port 7985
2. Wait for you to login via the web UI
3. Dynamically start the WebDAV server after successful login

The WebDAV server is dynamically managed without requiring container restarts:

- When you login, the WebDAV server starts automatically
- If tokens expire, the WebDAV server stops until new credentials are provided
- When you logout, the WebDAV server stops until you login again

This ensures the service is always available even without initial credentials and provides a smooth experience without container restarts.

### Security Note

The admin interface contains sensitive login functionality. If you're exposing the container outside your local network, consider:

1. Using a reverse proxy with HTTPS
2. Adding HTTP basic authentication
3. Only exposing the WebDAV port (7984) and keeping the admin interface (7985) on localhost or behind a firewall

## Building the image locally

If you want to build the image yourself:

```bash
git clone https://github.com/StollD/proton-webdav-bridge.git
cd proton-webdav-bridge
docker build -t proton-webdav-bridge .
```

Then run it using the same commands as above, replacing `ghcr.io/stolld/proton-webdav-bridge:latest` with `proton-webdav-bridge`.
