# WhatsApp API

API WhatsApp não-oficial com painel administrativo. Permite gerenciar múltiplas instâncias, enviar mensagens, gerenciar grupos, campanhas em massa e muito mais.

## 🚀 Quick Start

### 1. Pré-requisitos

- Node.js 18+
- PostgreSQL
- Redis
- Docker (opcional)

### 2. Instalação

```bash
# Clonar o projeto
cd "api whatsapp"

# Instalar dependências
npm install

# Copiar arquivo de ambiente
cp .env.example .env

# Editar o .env com suas configurações
```

### 3. Configurar Banco de Dados

**Opção A: Com Docker (recomendado)**

```bash
# Subir PostgreSQL e Redis
docker-compose up -d postgres redis

# Rodar migrations
npm run db:push
```

**Opção B: Sem Docker**

Configure as variáveis `DATABASE_URL` e `REDIS_URL` no `.env` para seus servidores locais.

```bash
# Rodar migrations
npm run db:push
```

### 4. Rodar o Servidor

```bash
# Modo desenvolvimento
npm run dev

# Modo produção
npm run build
npm start
```

O servidor estará rodando em `http://localhost:3000`

---

## 📖 Uso da API

### Autenticação

#### Registrar usuário
```bash
curl -X POST http://localhost:3000/auth/register \
  -H "Content-Type: application/json" \
  -d '{"email": "user@example.com", "password": "123456"}'
```

#### Login
```bash
curl -X POST http://localhost:3000/auth/login \
  -H "Content-Type: application/json" \
  -d '{"email": "user@example.com", "password": "123456"}'
```

Guarde o `token` retornado para usar nas próximas requisições.

---

### Gerenciar Instâncias

#### Criar instância
```bash
curl -X POST http://localhost:3000/admin/instance \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer SEU_TOKEN" \
  -d '{"name": "Minha Instância"}'
```

Guarde o `token` da instância para enviar mensagens.

#### Listar instâncias
```bash
curl http://localhost:3000/admin/instances \
  -H "Authorization: Bearer SEU_TOKEN"
```

---

### Conectar ao WhatsApp

#### Conectar (gera QR Code)
```bash
curl -X POST http://localhost:3000/instance/INSTANCE_ID/connect \
  -H "Authorization: Bearer SEU_TOKEN"
```

#### Ver QR Code
```bash
curl http://localhost:3000/instance/INSTANCE_ID/qr \
  -H "Authorization: Bearer SEU_TOKEN"
```

O QR Code é retornado em base64. Use para escanear com WhatsApp.

#### Verificar status
```bash
curl http://localhost:3000/instance/INSTANCE_ID/status \
  -H "Authorization: Bearer SEU_TOKEN"
```

---

### Enviar Mensagens

Use o **token da instância** (X-Instance-Token) para enviar mensagens.

#### Enviar texto
```bash
curl -X POST http://localhost:3000/message/text \
  -H "Content-Type: application/json" \
  -H "X-Instance-Token: TOKEN_DA_INSTANCIA" \
  -d '{
    "to": "5511999999999",
    "text": "Olá! Esta é uma mensagem de teste."
  }'
```

#### Enviar imagem
```bash
curl -X POST http://localhost:3000/message/media \
  -H "Content-Type: application/json" \
  -H "X-Instance-Token: TOKEN_DA_INSTANCIA" \
  -d '{
    "to": "5511999999999",
    "mediaUrl": "https://example.com/image.jpg",
    "caption": "Veja esta imagem!"
  }'
```

#### Enviar localização
```bash
curl -X POST http://localhost:3000/message/location \
  -H "Content-Type: application/json" \
  -H "X-Instance-Token: TOKEN_DA_INSTANCIA" \
  -d '{
    "to": "5511999999999",
    "latitude": -23.5505,
    "longitude": -46.6333,
    "description": "São Paulo, SP"
  }'
```

---

### Grupos

#### Criar grupo
```bash
curl -X POST http://localhost:3000/group/create \
  -H "Content-Type: application/json" \
  -H "X-Instance-Token: TOKEN_DA_INSTANCIA" \
  -d '{
    "name": "Meu Grupo",
    "participants": ["5511999999999", "5511888888888"]
  }'
```

#### Listar grupos
```bash
curl http://localhost:3000/groups \
  -H "X-Instance-Token: TOKEN_DA_INSTANCIA"
```

---

### Campanhas em Massa

#### Criar campanha simples
```bash
curl -X POST http://localhost:3000/campaign/simple \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer SEU_TOKEN" \
  -d '{
    "name": "Black Friday",
    "instanceId": "INSTANCE_ID",
    "message": {
      "type": "text",
      "text": "🔥 Promoção Black Friday! 50% OFF"
    },
    "recipients": ["5511999999999", "5511888888888"],
    "delay": 5000
  }'
```

#### Iniciar campanha
```bash
curl -X POST http://localhost:3000/campaign/CAMPAIGN_ID/start \
  -H "Authorization: Bearer SEU_TOKEN"
```

#### Pausar campanha
```bash
curl -X POST http://localhost:3000/campaign/CAMPAIGN_ID/control \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer SEU_TOKEN" \
  -d '{"action": "pause"}'
```

---

### Webhooks

Configure webhooks para receber eventos em tempo real.

#### Configurar webhook da instância
```bash
curl -X POST http://localhost:3000/instance/INSTANCE_ID/webhook \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer SEU_TOKEN" \
  -d '{
    "webhookUrl": "https://seu-servidor.com/webhook",
    "webhookEvents": ["message", "message_ack"]
  }'
```

#### Server-Sent Events (SSE)
```bash
curl http://localhost:3000/sse/INSTANCE_ID \
  -H "X-Instance-Token: TOKEN_DA_INSTANCIA"
```

---

## 📚 Endpoints Disponíveis

### Autenticação
| Método | Endpoint | Descrição |
|--------|----------|-----------|
| POST | /auth/register | Registrar usuário |
| POST | /auth/login | Login |
| GET | /auth/me | Info do usuário atual |

### Administração
| Método | Endpoint | Descrição |
|--------|----------|-----------|
| POST | /admin/instance | Criar instância |
| GET | /admin/instances | Listar instâncias |
| GET | /admin/instance/:id | Detalhes da instância |
| POST | /admin/instance/:id/update | Atualizar instância |
| DELETE | /admin/instance/:id | Deletar instância |
| GET | /admin/webhook | Ver webhook global |
| POST | /admin/webhook | Configurar webhook global |
| GET | /admin/stats | Estatísticas do sistema |

### Instância
| Método | Endpoint | Descrição |
|--------|----------|-----------|
| POST | /instance/:id/connect | Conectar ao WhatsApp |
| POST | /instance/:id/disconnect | Desconectar |
| POST | /instance/:id/logout | Logout (remove sessão) |
| GET | /instance/:id/status | Status da conexão |
| GET | /instance/:id/qr | QR Code |
| GET | /instance/:id/qr/stream | QR Code via SSE |

### Mensagens
| Método | Endpoint | Descrição |
|--------|----------|-----------|
| POST | /message/text | Enviar texto |
| POST | /message/media | Enviar mídia |
| POST | /message/location | Enviar localização |
| POST | /message/contact | Enviar contato |
| POST | /message/react | Reagir a mensagem |
| POST | /message/delete | Deletar mensagem |
| POST | /message/search | Buscar mensagens |
| POST | /message/download | Download de mídia |

---

## 📷 Recebendo Mídia (Imagem, Áudio, Vídeo, Documento)

Quando você recebe uma mensagem de mídia via webhook, ela já vem com o conteúdo em **base64**:

### Payload do Webhook com Mídia:
```json
{
  "event": "message",
  "instanceId": "sua-instancia-id",
  "timestamp": 1736413125,
  "data": {
    "id": "3EB0B9A53DBA68DEE47918",
    "from": "5511999999999@s.whatsapp.net",
    "to": "5585999999999@s.whatsapp.net",
    "type": "image",
    "body": "legenda da imagem",
    "timestamp": 1736413125,
    "fromMe": false,
    "isGroup": false,
    "pushName": "João",
    "mediaBase64": "data:image/jpeg;base64,/9j/4AAQSkZJRg...",
    "mimetype": "image/jpeg",
    "caption": "legenda da imagem",
    "fileName": ""
  }
}
```

### Tipos de Mídia Suportados:

| Tipo | `type` | `mimetype` (exemplos) |
|------|--------|----------------------|
| Imagem | `image` | `image/jpeg`, `image/png`, `image/webp` |
| Vídeo | `video` | `video/mp4`, `video/3gpp` |
| Áudio | `audio` | `audio/ogg; codecs=opus`, `audio/mpeg` |
| Documento | `document` | `application/pdf`, `application/msword` |
| Sticker | `sticker` | `image/webp` |

### Exemplo em Python:
```python
import base64

def handle_webhook(data):
    if data.get('mediaBase64'):
        # Decodificar base64
        media_bytes = base64.b64decode(data['mediaBase64'])
        mimetype = data.get('mimetype', 'application/octet-stream')
        
        # Salvar arquivo
        extension = mimetype.split('/')[-1]
        with open(f"media.{extension}", "wb") as f:
            f.write(media_bytes)
        
        # Ou processar com IA
        # response = openai.vision_preview(image=media_bytes)
```

### Exemplo em Node.js:
```javascript
function handleWebhook(data) {
    if (data.mediaBase64) {
        const buffer = Buffer.from(data.mediaBase64, 'base64');
        const mimetype = data.mimetype || 'application/octet-stream';
        
        // Salvar arquivo
        fs.writeFileSync(`media.${mimetype.split('/')[1]}`, buffer);
    }
}
```

---

## 📥 Download de Mídia (Endpoint)

Caso precise baixar mídia posteriormente (quando você tem os metadados da mensagem):

#### Request:
```bash
curl -X POST http://localhost:3000/message/download \
  -H "Content-Type: application/json" \
  -H "X-Instance-Token: TOKEN_DA_INSTANCIA" \
  -d '{
    "instanceId": "sua-instancia-id",
    "url": "https://mmg.whatsapp.net/...",
    "directPath": "/v/t62.7114-24/...",
    "mediaKey": "base64-da-chave...",
    "fileEncSha256": "base64-do-hash...",
    "fileSha256": "base64-do-hash...",
    "fileLength": 12345,
    "mediaType": "image",
    "mimetype": "image/jpeg"
  }'
```

#### Response:
```json
{
  "success": true,
  "data": {
    "data": "base64-do-arquivo...",
    "mimetype": "image/jpeg",
    "size": 12345
  }
}
```

> **Nota:** Os campos `url`, `directPath`, `mediaKey`, etc. são fornecidos no evento de webhook original quando a mensagem é recebida.

### Contatos
| Método | Endpoint | Descrição |
|--------|----------|-----------|
| GET | /contacts | Listar contatos |
| POST | /contacts/list | Listar com paginação |
| POST | /contacts/details | Detalhes do contato |
| POST | /contacts/verify | Verificar números |
| POST | /contacts/block | Bloquear |
| POST | /contacts/unblock | Desbloquear |
| GET | /contacts/blocked | Listar bloqueados |

### Grupos
| Método | Endpoint | Descrição |
|--------|----------|-----------|
| POST | /group/create | Criar grupo |
| POST | /group/info | Info do grupo |
| GET | /groups | Listar grupos |
| POST | /group/participants/add | Adicionar participantes |
| POST | /group/participants/remove | Remover participantes |
| POST | /group/leave | Sair do grupo |
| POST | /group/invite-code | Obter link de convite |

### Chats
| Método | Endpoint | Descrição |
|--------|----------|-----------|
| GET | /chats | Listar chats |
| POST | /chats/search | Buscar chats |
| POST | /chat/archive | Arquivar |
| POST | /chat/pin | Fixar |
| POST | /chat/mute | Silenciar |
| POST | /chat/delete | Deletar |

### Campanhas
| Método | Endpoint | Descrição |
|--------|----------|-----------|
| GET | /campaigns | Listar campanhas |
| POST | /campaign/simple | Criar campanha simples |
| POST | /campaign/advanced | Criar campanha avançada |
| POST | /campaign/:id/start | Iniciar campanha |
| POST | /campaign/:id/control | Pausar/Retomar/Cancelar |
| DELETE | /campaign/:id | Deletar campanha |

---

## 🔧 Estrutura do Projeto

```
src/
├── config/
│   └── env.ts           # Variáveis de ambiente
├── lib/
│   ├── prisma.ts        # Cliente Prisma
│   ├── redis.ts         # Cliente Redis
│   ├── logger.ts        # Logger Pino
│   └── whatsapp.ts      # Gerenciador WhatsApp
├── middlewares/
│   ├── auth.ts          # Autenticação JWT
│   └── error.ts         # Handler de erros
├── modules/
│   ├── auth/            # Autenticação
│   ├── admin/           # Administração
│   ├── instance/        # Instâncias
│   ├── messages/        # Mensagens
│   ├── contacts/        # Contatos
│   ├── groups/          # Grupos
│   ├── chats/           # Chats
│   ├── profile/         # Perfil
│   ├── campaigns/       # Campanhas
│   └── webhooks/        # Webhooks
└── server.ts            # Entry point
```

---

## ⚠️ Avisos Importantes

1. **Uso não-oficial**: Esta API usa engenharia reversa do WhatsApp Web. Não é endossada pelo WhatsApp/Meta.

2. **Risco de ban**: O uso excessivo (spam, muitas mensagens) pode resultar em banimento da conta.

3. **WhatsApp Business**: Recomendamos usar contas WhatsApp Business para maior estabilidade.

4. **Recursos**: Cada instância consome ~300-500MB de RAM devido ao Chromium.

---

## 📄 Licença

ISC
