<div align="center">

# WhatsApp MCP Server

**Give AI assistants access to your WhatsApp conversations**

[![Go Version](https://img.shields.io/badge/Go-1.25%2B-00ADD8?style=flat&logo=go)](https://go.dev/)
[![MCP Protocol](https://img.shields.io/badge/MCP-Compatible-7C3AED?style=flat)](https://modelcontextprotocol.io)
[![Docker](https://img.shields.io/badge/Docker-Ready-2496ED?style=flat&logo=docker&logoColor=white)](https://www.docker.com/)
[![License: GPL v3](https://img.shields.io/badge/License-GPLv3-blue.svg?style=flat)](LICENSE)
[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/felipeadeildo/whatsapp-mcp)

*Built with [whatsmeow](https://github.com/tulir/whatsmeow) and [mcp-go](https://github.com/mark3labs/mcp-go)*

[Features](#-features) • [Quick Start](#-quick-start) • [Architecture](#-architecture) • [MCP Integration](#-mcp-integration)

</div>

## 🎯 What is This?

A **Model Context Protocol (MCP) server** that bridges WhatsApp and AI assistants like Claude. It exposes your WhatsApp messages through standardized MCP tools, prompts, and resources - allowing AI to read, search, and send messages on your behalf.

**The Vision:** Let AI handle your WhatsApp conversations intelligently, with full context and natural language understanding.

```
You: "Summarize what João said about the budget meeting"
AI:  *searches all your chats* → "João mentioned in the Tech Team group..."

You: "Reply to Maria's last message and schedule lunch"
AI:  *reads context, sends reply* → "Sent! I've proposed Thursday at noon"
```

## ✨ Features

### Core Capabilities

- **📱 Full WhatsApp Integration** - Connect to WhatsApp Web using your existing account
- **💾 Local-First Storage** - All messages stored in SQLite, synced in real-time
- **🔍 Powerful Search** - Pattern matching, cross-chat queries, sender filtering
- **⏱️ Timezone Support** - Messages displayed in your local timezone
- **📥 On-Demand Loading** - Fetch older messages from WhatsApp servers as needed
- **🔐 Secure by Design** - Per-setup API keys, isolated WhatsApp storage, HTTPS ready

### MCP Features

This server implements the full MCP specification with:

- **7 Tools** for WhatsApp operations
- **4 Prompts** for common workflows
- **4 Resources** for interactive guides
- **Server Instructions** for optimal AI interactions

#### Tools

| Tool | Purpose | Highlights |
|------|---------|-----------|
| `list_chats` | Browse conversations | Ordered by recent activity |
| `get_chat_messages` | Read specific chat | Pagination, sender filtering |
| `search_messages` | Search across all chats | Pattern matching, wildcards |
| `find_chat` | Locate chat by name | Fuzzy search support |
| `send_message` | Send WhatsApp messages | To any chat or group |
| `load_more_messages` | Fetch older history | On-demand from servers |
| `get_my_info` | Get your profile info | JID, name, status, picture |

#### Prompts

Pre-built workflows that guide AI assistants:

- **`search_person_messages`** - Find ALL messages from someone across all chats
- **`get_context_about_person`** - Comprehensive analysis of someone's messages
- **`analyze_conversation`** - Summarize recent chat activity
- **`search_keyword`** - Find specific topics across conversations

#### Resources

Interactive documentation embedded in the MCP server:

- **Cross-Chat Search Guide** - Master advanced search workflows
- **Workflow Guide** - Common operations and best practices
- **JID Format Guide** - Understanding WhatsApp identifiers
- **Search Patterns Guide** - Wildcards and pattern matching

## 🏗️ Architecture

```mermaid
graph TB
    subgraph "AI Client"
        A[AI Assistant <br/> e.g., Claude Web]
    end

    subgraph "WhatsApp MCP Server"
        B[MCP HTTP Server :8080]
        S[/Setup UI<br/>/setup/]
        C[MCP Layer<br/>per tenant]
        D[WhatsApp Client<br/>per tenant]
        E[(SQLite Database<br/>per tenant)]

        B -->|/setup creates tenant + QR| S
        B -->|/mcp/{tenant_id}| C
        B -->|/health| B

        C -->|Tools| C1[list_chats<br/>get_chat_messages<br/>search_messages<br/>find_chat<br/>send_message<br/>load_more_messages<br/>get_my_info]
        C -->|Prompts| C2[search_person_messages<br/>get_context_about_person<br/>analyze_conversation<br/>search_keyword]
        C -->|Resources| C3[Workflow Guides<br/>Search Patterns<br/>JID Format]

        C1 -.->|read/write| E
        C1 -.->|send| D

        D -->|sync messages| E
        D <-->|WhatsApp Protocol| F
    end

    subgraph "WhatsApp"
        F[WhatsApp Servers]
    end

    A <-->|Streamable HTTP<br/>Tenant API Key Auth| B

    style A fill:#4A90E2,stroke:#2E5C8A,stroke-width:2px,color:#000
    style B fill:#F5A623,stroke:#C67E1B,stroke-width:2px,color:#000
    style C fill:#9013FE,stroke:#6B0FC7,stroke-width:2px,color:#fff
    style C1 fill:#50E3C2,stroke:#3AAA94,stroke-width:2px,color:#000
    style C2 fill:#BD10E0,stroke:#9012FE,stroke-width:2px,color:#fff
    style C3 fill:#F5A623,stroke:#C67E1B,stroke-width:2px,color:#000
    style D fill:#50E3C2,stroke:#3AAA94,stroke-width:2px,color:#000
    style E fill:#E85D75,stroke:#B5475C,stroke-width:2px,color:#fff
    style F fill:#25D366,stroke:#1DA851,stroke-width:2px,color:#000
```

### How It Works

1. **Initial Sync** - WhatsApp sends message history on first connection
2. **Real-Time Updates** - All new messages automatically stored in SQLite
3. **MCP Exposure** - Tools, prompts, and resources expose functionality to AI
4. **On-Demand Loading** - Fetch older messages from WhatsApp when needed
5. **AI Integration** - Claude (or any MCP client) accesses WhatsApp through standardized protocol

## 🚀 Quick Start

### Prerequisites

- **Go 1.25.5+** (for local setup) or **Docker** (recommended)
- **WhatsApp account** (will be linked via QR code)
- **MCP-compatible AI client** (Claude, Cursor, etc.)

### Option 1: Docker Setup (Recommended)

1. **Clone and configure**
   ```bash
   git clone https://github.com/felipeadeildo/whatsapp-mcp
   cd whatsapp-mcp
   cp .env.example .env
   # Edit .env with your settings (API key, timezone, etc.)
   ```

2. **Start the server**
   ```bash
   docker compose up -d
   ```

3. **Create a WhatsApp setup**
   Open `http://localhost:8080/setup`, click **Create WhatsApp Setup**, and scan the QR code with WhatsApp:
   `Settings → Linked Devices → Link a Device`.

   The setup page shows:
   - A tenant ID
   - A tenant-specific MCP URL
   - A tenant-specific API key

4. **Verify it's running**
   ```bash
   curl http://localhost:8080/health
   # Expected: {"status":"ok"}
   ```

### Option 2: Local Setup

1. **Install dependencies**
   ```bash
   git clone https://github.com/felipeadeildo/whatsapp-mcp
   cd whatsapp-mcp
   go mod download
   ```

2. **Configure environment**
   ```bash
   cp .env.example .env
   # Edit .env with your settings
   ```

3. **Run the server**
   ```bash
   go run main.go
   ```

4. **Link WhatsApp**
   Open `http://localhost:8080/setup`, create a setup, and scan the QR code.

## 🔌 MCP Integration

### Connect to Claude Desktop

Add to your Claude Desktop config (`~/Library/Application Support/Claude/claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "whatsapp": {
      "url": "http://localhost:8080/mcp/your-tenant-id",
      "transport": "http",
      "headers": {
        "Authorization": "Bearer your-tenant-api-key"
      }
    }
  }
}
```

### Connect to Other MCP Clients

The server exposes an HTTP+SSE endpoint compatible with any MCP client:

- **Setup URL:** `http://localhost:8080/setup`
- **MCP URL:** `http://localhost:8080/mcp/{TENANT_ID}`
- **Transport:** Streamable HTTP
- **Direct API-key authentication:** `Authorization: Bearer {TENANT_API_KEY}` or `X-API-Key: {TENANT_API_KEY}`
- **Standard MCP OAuth:** Supported per tenant. Unauthenticated MCP requests return a `WWW-Authenticate` challenge with tenant-specific protected-resource metadata. The OAuth login page asks for that tenant's API key and issues an access token bound to `http://host/mcp/{TENANT_ID}`.

Each WhatsApp setup has its own tenant ID, WhatsApp auth database, message database, media directory, and API key. A tenant API key can only call that tenant's MCP endpoint.

OAuth discovery endpoints are tenant-specific:

- Protected resource metadata: `/.well-known/oauth-protected-resource/mcp/{TENANT_ID}`
- Authorization server metadata: `/oauth/{TENANT_ID}/.well-known/oauth-authorization-server`
- Dynamic client registration: `/oauth/{TENANT_ID}/register`
- Authorization: `/oauth/{TENANT_ID}/authorize`
- Token exchange: `/oauth/{TENANT_ID}/token`

## 🎨 Usage Examples

Once connected, your AI assistant can:

### Search for People
```
You: "Find all messages from Arthur across all my chats"
AI: [Uses search_person_messages prompt]
    → Finds messages in DMs, groups, everywhere
    → Analyzes communication patterns
    → Provides context about Arthur
```

### Analyze Conversations
```
You: "What did we discuss in the Tech Team group this week?"
AI: [Uses analyze_conversation prompt]
    → Reads recent messages
    → Summarizes key topics
    → Lists action items and deadlines
```

### Smart Messaging
```
You: "Tell Maria I'll be 10 minutes late"
AI: [Uses find_chat + send_message]
    → Finds Maria's chat
    → Sends contextual message
    → Confirms delivery
```

### Deep Search
```
You: "Find all mentions of 'budget meeting' in any chat"
AI: [Uses search_keyword prompt]
    → Searches across all conversations
    → Shows context around each mention
    → Orders by relevance/date
```

## 📊 Data & Privacy

### Local Storage

All tenant data is stored under `./data/tenants/{tenant_id}/`:
- **`db/messages.db`** - SQLite database with messages and chats
- **`db/whatsapp_auth.db`** - WhatsApp session credentials
- **`media/`** - Downloaded media files
- **`whatsapp.log`** - WhatsApp client logs

Tenant metadata is stored in `./data/tenants/registry.json`. API keys and setup tokens are stored as SHA-256 hashes, not plaintext.

For Railway deployments, mount `./data/tenants` on persistent storage. A shared Railway database is not required for tenant isolation because each tenant already has separate SQLite and WhatsApp auth files; if the filesystem is ephemeral, WhatsApp sessions and tenant API-key hashes will be lost on restart.

**⚠️ Important:** Database files contain sensitive data. Keep them secure (file permissions `600`) and backed up.

## 🛣️ Roadmap

### ✅ Implemented

- [x] WhatsApp Web integration via whatsmeow
- [x] Real-time message sync to SQLite
- [x] MCP server with Streamable HTTP transport
- [x] Pattern matching and wildcards
- [x] Sender filtering and cross-chat search
- [x] Timestamp-based pagination
- [x] Timezone support
- [x] On-demand message loading from servers
- [x] Docker deployment (with healthcheck!)

### 🚧 Planned

- [ ] **Media Support**
  - Voice message transcription
  - Image OCR and analysis
  - Video metadata extraction
  - Document parsing
  - Contact card handling

- [ ] **GraphRAG Integration**
  - Entity extraction from conversations
  - Relationship mapping between contacts
  - Semantic search capabilities
  - Context-aware recommendations

- [ ] **Enhanced Tools**
  - Mark messages as read
  - React to messages (emoji reactions)
  - Send media files
  - Group management (create, members)
  - Status updates
  - Account management (profile picture, name)

- [ ] **Analytics** (maybe)
  - Message statistics
  - Conversation insights
  - Response time tracking

## 📚 Documentation

### MCP Resources (Built-In)

The server includes interactive guides accessible through MCP:
- **Workflow Guide** - Common operations and patterns
- **Cross-Chat Search** - Master advanced search techniques
- **JID Format Guide** - Understanding WhatsApp identifiers
- **Search Patterns** - Wildcards and pattern matching

AI assistants can access these guides through the MCP Resources API.

### Environment Variables

See `.env.example` and be happy!

## 🤝 Contributing

This is a personal project I maintain for daily use. Contributions are welcome!

See [CONTRIBUTING.md](CONTRIBUTING.md) for:
- Development setup and workflow
- Project structure (main server vs migration CLI)
- Database migration system
- Code style guidelines

Quick start:
1. Fork the repository
2. Create your feature branch
3. Follow the guidelines in CONTRIBUTING.md
4. Submit a pull request

## ⚠️ Disclaimer

This project is **not affiliated with WhatsApp or Meta**. It uses the unofficial WhatsApp Web API through the whatsmeow library. Use at your own risk.

**Important Notes:**
- WhatsApp may change their API at any time
- Using unofficial APIs may violate WhatsApp's Terms of Service
- This is provided as-is with no warranties
- Keep your session data secure

---

<div align="center">

**Built with ❤️ for the MCP community**

[Report Bug](https://github.com/felipeadeildo/whatsapp-mcp/issues) • [Request Feature](https://github.com/felipeadeildo/whatsapp-mcp/issues)

</div>
