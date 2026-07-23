import sqlite3
from datetime import datetime
from dataclasses import dataclass
from typing import Any, Dict, Optional, List, Tuple
import os
import requests
import json
import audio
from macos_contacts import search_macos_contacts

DEFAULT_STORE_DIR = os.path.join(
    os.path.dirname(os.path.abspath(__file__)), "..", "whatsapp-bridge", "store"
)
STORE_DIR = os.path.abspath(os.environ.get("WHATSAPP_STORE_DIR", DEFAULT_STORE_DIR))
MESSAGES_DB_PATH = os.path.join(STORE_DIR, "messages.db")
WHATSAPP_API_BASE_URL = os.environ.get(
    "WHATSAPP_BRIDGE_URL", "http://127.0.0.1:8741/api"
).rstrip("/")
BRIDGE_REQUEST_TIMEOUT_SECONDS = 30
MEDIA_REQUEST_TIMEOUT_SECONDS = 120
MAX_PAGE_SIZE = 100
MAX_PAGE_NUMBER = 1_000
MAX_CONTEXT_MESSAGES = 50


def _clamp_integer(value: int, default: int, minimum: int, maximum: int) -> int:
    try:
        parsed = int(value)
    except (TypeError, ValueError, OverflowError):
        return default
    return max(minimum, min(parsed, maximum))

@dataclass
class Message:
    timestamp: datetime
    sender: str
    content: str
    is_from_me: bool
    chat_jid: str
    id: str
    chat_name: Optional[str] = None
    media_type: Optional[str] = None

@dataclass
class Chat:
    jid: str
    name: Optional[str]
    last_message_time: Optional[datetime]
    last_message: Optional[str] = None
    last_sender: Optional[str] = None
    last_is_from_me: Optional[bool] = None

    @property
    def is_group(self) -> bool:
        """Determine if chat is a group based on JID pattern."""
        return self.jid.endswith("@g.us")

@dataclass
class Contact:
    phone_number: str
    name: Optional[str]
    jid: Optional[str]
    source: str = "whatsapp"
    raw_phone_number: Optional[str] = None
    has_whatsapp_chat: bool = True

@dataclass
class MessageContext:
    message: Message
    before: List[Message]
    after: List[Message]


def get_bridge_status() -> dict:
    """Report live bridge connectivity and local message-cache freshness."""
    result = {
        "bridge_url": WHATSAPP_API_BASE_URL,
        "bridge_reachable": False,
        "connected": False,
        "logged_in": False,
        "database_path": MESSAGES_DB_PATH,
        "database_exists": os.path.isfile(MESSAGES_DB_PATH),
        "latest_stored_message_time": None,
    }

    if result["database_exists"]:
        result["database_modified_at"] = datetime.fromtimestamp(
            os.path.getmtime(MESSAGES_DB_PATH)
        ).astimezone().isoformat()
        try:
            with sqlite3.connect(MESSAGES_DB_PATH) as conn:
                row = conn.execute("SELECT MAX(timestamp) FROM messages").fetchone()
                if row:
                    result["latest_stored_message_time"] = row[0]
        except sqlite3.Error as error:
            result["database_error"] = str(error)

    try:
        response = requests.get(f"{WHATSAPP_API_BASE_URL}/health", timeout=2)
        response.raise_for_status()
        health = response.json()
        result.update(health)
        result["bridge_reachable"] = True
    except (requests.RequestException, json.JSONDecodeError) as error:
        result["bridge_error"] = str(error)

    result["reads_may_be_stale"] = not (
        result.get("bridge_reachable") and result.get("connected") and result.get("logged_in")
    )
    return result


def _bridge_api_request(
    method: str,
    path: str,
    payload: Optional[Dict[str, Any]] = None,
    params: Optional[Dict[str, Any]] = None,
    timeout: int = BRIDGE_REQUEST_TIMEOUT_SECONDS,
) -> Dict[str, Any]:
    try:
        response = requests.request(
            method,
            f"{WHATSAPP_API_BASE_URL}/{path.lstrip('/')}",
            json=payload,
            params=params,
            timeout=timeout,
        )
        try:
            result = response.json()
        except json.JSONDecodeError:
            result = {"success": False, "message": response.text or "Invalid bridge response"}
        if response.ok:
            return result
        return {
            "success": False,
            "message": result.get("message", f"Bridge returned HTTP {response.status_code}"),
            "status_code": response.status_code,
        }
    except requests.RequestException as error:
        return {"success": False, "message": f"Bridge request failed: {error}"}

def get_sender_name(sender_jid: str) -> str:
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        # First try matching by exact JID
        cursor.execute("""
            SELECT name
            FROM chats
            WHERE jid = ?
            LIMIT 1
        """, (sender_jid,))
        
        result = cursor.fetchone()
        
        # If no result, try looking for the number within JIDs
        if not result:
            # Extract the phone number part if it's a JID
            if '@' in sender_jid:
                phone_part = sender_jid.split('@')[0]
            else:
                phone_part = sender_jid
                
            cursor.execute("""
                SELECT name
                FROM chats
                WHERE jid LIKE ?
                LIMIT 1
            """, (f"%{phone_part}%",))
            
            result = cursor.fetchone()
        
        if result and result[0]:
            return result[0]
        else:
            return sender_jid
        
    except sqlite3.Error as e:
        print(f"Database error while getting sender name: {e}")
        return sender_jid
    finally:
        if 'conn' in locals():
            conn.close()

def format_message(message: Message, show_chat_info: bool = True) -> None:
    """Print a single message with consistent formatting."""
    output = ""
    
    if show_chat_info and message.chat_name:
        output += f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] Chat: {message.chat_name} "
    else:
        output += f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] "
        
    content_prefix = ""
    if hasattr(message, 'media_type') and message.media_type:
        content_prefix = f"[{message.media_type} - Message ID: {message.id} - Chat JID: {message.chat_jid}] "
    
    try:
        sender_name = get_sender_name(message.sender) if not message.is_from_me else "Me"
        output += f"From: {sender_name}: {content_prefix}{message.content}\n"
    except Exception as e:
        print(f"Error formatting message: {e}")
    return output

def format_messages_list(messages: List[Message], show_chat_info: bool = True) -> None:
    output = ""
    if not messages:
        output += "No messages to display."
        return output
    
    for message in messages:
        output += format_message(message, show_chat_info)
    return output

def list_messages(
    after: Optional[str] = None,
    before: Optional[str] = None,
    sender_phone_number: Optional[str] = None,
    chat_jid: Optional[str] = None,
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_context: bool = True,
    context_before: int = 1,
    context_after: int = 1
) -> List[Message]:
    """Get messages matching the specified criteria with optional context."""
    limit = _clamp_integer(limit, 20, 1, MAX_PAGE_SIZE)
    page = _clamp_integer(page, 0, 0, MAX_PAGE_NUMBER)
    context_before = _clamp_integer(
        context_before, 1, 0, MAX_CONTEXT_MESSAGES
    )
    context_after = _clamp_integer(context_after, 1, 0, MAX_CONTEXT_MESSAGES)
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        # Build base query
        query_parts = ["SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.media_type FROM messages"]
        query_parts.append("JOIN chats ON messages.chat_jid = chats.jid")
        where_clauses = []
        params = []
        
        # Add filters
        if after:
            try:
                after = datetime.fromisoformat(after)
            except ValueError:
                raise ValueError(f"Invalid date format for 'after': {after}. Please use ISO-8601 format.")
            
            where_clauses.append("messages.timestamp > ?")
            params.append(after)

        if before:
            try:
                before = datetime.fromisoformat(before)
            except ValueError:
                raise ValueError(f"Invalid date format for 'before': {before}. Please use ISO-8601 format.")
            
            where_clauses.append("messages.timestamp < ?")
            params.append(before)

        if sender_phone_number:
            where_clauses.append("messages.sender = ?")
            params.append(sender_phone_number)
            
        if chat_jid:
            where_clauses.append("messages.chat_jid = ?")
            params.append(chat_jid)
            
        if query:
            where_clauses.append("LOWER(messages.content) LIKE LOWER(?)")
            params.append(f"%{query}%")
            
        if where_clauses:
            query_parts.append("WHERE " + " AND ".join(where_clauses))
            
        # Add pagination
        offset = page * limit
        query_parts.append("ORDER BY messages.timestamp DESC")
        query_parts.append("LIMIT ? OFFSET ?")
        params.extend([limit, offset])
        
        cursor.execute(" ".join(query_parts), tuple(params))
        messages = cursor.fetchall()
        
        result = []
        for msg in messages:
            message = Message(
                timestamp=datetime.fromisoformat(msg[0]),
                sender=msg[1],
                chat_name=msg[2],
                content=msg[3],
                is_from_me=msg[4],
                chat_jid=msg[5],
                id=msg[6],
                media_type=msg[7]
            )
            result.append(message)
            
        if include_context and result:
            # Add context for each message
            messages_with_context = []
            for msg in result:
                context = get_message_context(msg.id, context_before, context_after)
                messages_with_context.extend(context.before)
                messages_with_context.append(context.message)
                messages_with_context.extend(context.after)
            
            return format_messages_list(messages_with_context, show_chat_info=True)
            
        # Format and display messages without context
        return format_messages_list(result, show_chat_info=True)    
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        return []
    finally:
        if 'conn' in locals():
            conn.close()


def get_message_context(
    message_id: str,
    before: int = 5,
    after: int = 5
) -> MessageContext:
    """Get context around a specific message."""
    before = _clamp_integer(before, 5, 0, MAX_CONTEXT_MESSAGES)
    after = _clamp_integer(after, 5, 0, MAX_CONTEXT_MESSAGES)
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        # Get the target message first
        cursor.execute("""
            SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.chat_jid, messages.media_type
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.id = ?
        """, (message_id,))
        msg_data = cursor.fetchone()
        
        if not msg_data:
            raise ValueError(f"Message with ID {message_id} not found")
            
        target_message = Message(
            timestamp=datetime.fromisoformat(msg_data[0]),
            sender=msg_data[1],
            chat_name=msg_data[2],
            content=msg_data[3],
            is_from_me=msg_data[4],
            chat_jid=msg_data[5],
            id=msg_data[6],
            media_type=msg_data[8]
        )
        
        # Get messages before
        cursor.execute("""
            SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.media_type
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.chat_jid = ? AND messages.timestamp < ?
            ORDER BY messages.timestamp DESC
            LIMIT ?
        """, (msg_data[7], msg_data[0], before))
        
        before_messages = []
        for msg in cursor.fetchall():
            before_messages.append(Message(
                timestamp=datetime.fromisoformat(msg[0]),
                sender=msg[1],
                chat_name=msg[2],
                content=msg[3],
                is_from_me=msg[4],
                chat_jid=msg[5],
                id=msg[6],
                media_type=msg[7]
            ))
        
        # Get messages after
        cursor.execute("""
            SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.media_type
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.chat_jid = ? AND messages.timestamp > ?
            ORDER BY messages.timestamp ASC
            LIMIT ?
        """, (msg_data[7], msg_data[0], after))
        
        after_messages = []
        for msg in cursor.fetchall():
            after_messages.append(Message(
                timestamp=datetime.fromisoformat(msg[0]),
                sender=msg[1],
                chat_name=msg[2],
                content=msg[3],
                is_from_me=msg[4],
                chat_jid=msg[5],
                id=msg[6],
                media_type=msg[7]
            ))
        
        return MessageContext(
            message=target_message,
            before=before_messages,
            after=after_messages
        )
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        raise
    finally:
        if 'conn' in locals():
            conn.close()


def list_chats(
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active"
) -> List[Chat]:
    """Get chats matching the specified criteria."""
    limit = _clamp_integer(limit, 20, 1, MAX_PAGE_SIZE)
    page = _clamp_integer(page, 0, 0, MAX_PAGE_NUMBER)
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        # Build base query
        query_parts = ["""
            SELECT 
                chats.jid,
                chats.name,
                chats.last_message_time,
                messages.content as last_message,
                messages.sender as last_sender,
                messages.is_from_me as last_is_from_me
            FROM chats
        """]
        
        if include_last_message:
            query_parts.append("""
                LEFT JOIN messages ON chats.jid = messages.chat_jid 
                AND chats.last_message_time = messages.timestamp
            """)
            
        where_clauses = []
        params = []
        
        if query:
            where_clauses.append("(LOWER(chats.name) LIKE LOWER(?) OR chats.jid LIKE ?)")
            params.extend([f"%{query}%", f"%{query}%"])
            
        if where_clauses:
            query_parts.append("WHERE " + " AND ".join(where_clauses))
            
        # Add sorting
        order_by = "chats.last_message_time DESC" if sort_by == "last_active" else "chats.name"
        query_parts.append(f"ORDER BY {order_by}")
        
        # Add pagination
        offset = (page ) * limit
        query_parts.append("LIMIT ? OFFSET ?")
        params.extend([limit, offset])
        
        cursor.execute(" ".join(query_parts), tuple(params))
        chats = cursor.fetchall()
        
        result = []
        for chat_data in chats:
            chat = Chat(
                jid=chat_data[0],
                name=chat_data[1],
                last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
                last_message=chat_data[3],
                last_sender=chat_data[4],
                last_is_from_me=chat_data[5]
            )
            result.append(chat)
            
        return result
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        return []
    finally:
        if 'conn' in locals():
            conn.close()


def _normalize_phone_number(phone_number: str) -> str:
    digits = "".join(character for character in phone_number if character.isdigit())
    if digits.startswith("00"):
        digits = digits[2:]
    return digits


def _search_whatsapp_contacts(query: str) -> List[Contact]:
    """Search direct-chat contacts already known to the WhatsApp message cache."""
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        # Split query into characters to support partial matching
        search_pattern = '%' +query + '%'
        
        cursor.execute("""
            SELECT DISTINCT 
                jid,
                name
            FROM chats
            WHERE 
                (LOWER(name) LIKE LOWER(?) OR LOWER(jid) LIKE LOWER(?))
                AND jid NOT LIKE '%@g.us'
            ORDER BY name, jid
            LIMIT 50
        """, (search_pattern, search_pattern))
        
        contacts = cursor.fetchall()
        
        result = []
        for contact_data in contacts:
            contact = Contact(
                phone_number=contact_data[0].split('@')[0],
                name=contact_data[1],
                jid=contact_data[0],
                source="whatsapp",
                has_whatsapp_chat=True,
            )
            result.append(contact)
            
        return result
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        return []
    finally:
        if 'conn' in locals():
            conn.close()


def search_contacts(query: str) -> List[Contact]:
    """Search known WhatsApp chats and merge optional macOS Contacts.app entries."""
    whatsapp_contacts = _search_whatsapp_contacts(query)
    macos_contacts, _ = search_macos_contacts(query, limit=50)

    merged_by_phone = {
        _normalize_phone_number(contact.phone_number): contact
        for contact in whatsapp_contacts
        if _normalize_phone_number(contact.phone_number)
    }

    for macos_contact in macos_contacts:
        raw_phone_number = macos_contact.get("phone_number") or ""
        normalized = _normalize_phone_number(raw_phone_number)
        if not normalized:
            continue

        existing = merged_by_phone.get(normalized)
        if existing:
            if not existing.name and macos_contact.get("name"):
                existing.name = macos_contact["name"]
            existing.source = "whatsapp+macos_contacts"
            existing.raw_phone_number = raw_phone_number
            continue

        merged_by_phone[normalized] = Contact(
            phone_number=normalized,
            name=macos_contact.get("name"),
            jid=None,
            source="macos_contacts",
            raw_phone_number=raw_phone_number,
            has_whatsapp_chat=False,
        )

    return sorted(
        merged_by_phone.values(),
        key=lambda contact: ((contact.name or "").lower(), contact.phone_number),
    )[:50]


def search_contacts_chats_and_groups(query: str, limit: int = 50) -> List[Dict[str, Any]]:
    """Return unified direct chats, groups, and address-book contacts."""
    chats = list_chats(query=query or None, limit=limit, include_last_message=True)
    contacts = search_contacts(query)
    results: List[Dict[str, Any]] = []
    seen_jids = set()
    seen_phones = set()

    for chat in chats:
        item_type = "group" if chat.is_group else "direct_chat"
        phone_number = chat.jid.split("@", 1)[0] if not chat.is_group else None
        results.append({
            "type": item_type,
            "jid": chat.jid,
            "phone_number": phone_number,
            "name": chat.name,
            "last_message_time": chat.last_message_time.isoformat() if chat.last_message_time else None,
            "last_message": chat.last_message,
            "has_whatsapp_chat": True,
            "source": "whatsapp",
        })
        seen_jids.add(chat.jid)
        if phone_number:
            seen_phones.add(_normalize_phone_number(phone_number))

    for contact in contacts:
        normalized = _normalize_phone_number(contact.phone_number)
        if (contact.jid and contact.jid in seen_jids) or normalized in seen_phones:
            continue
        results.append({
            "type": "contact",
            "jid": contact.jid,
            "phone_number": contact.phone_number,
            "raw_phone_number": contact.raw_phone_number,
            "name": contact.name,
            "has_whatsapp_chat": contact.has_whatsapp_chat,
            "source": contact.source,
        })

    return results[:limit]


def get_contact_chats(jid: str, limit: int = 20, page: int = 0) -> List[Chat]:
    """Get all chats involving the contact.
    
    Args:
        jid: The contact's JID to search for
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
    """
    limit = _clamp_integer(limit, 20, 1, MAX_PAGE_SIZE)
    page = _clamp_integer(page, 0, 0, MAX_PAGE_NUMBER)
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        cursor.execute("""
            SELECT DISTINCT
                c.jid,
                c.name,
                c.last_message_time,
                m.content as last_message,
                m.sender as last_sender,
                m.is_from_me as last_is_from_me
            FROM chats c
            JOIN messages m ON c.jid = m.chat_jid
            WHERE m.sender = ? OR c.jid = ?
            ORDER BY c.last_message_time DESC
            LIMIT ? OFFSET ?
        """, (jid, jid, limit, page * limit))
        
        chats = cursor.fetchall()
        
        result = []
        for chat_data in chats:
            chat = Chat(
                jid=chat_data[0],
                name=chat_data[1],
                last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
                last_message=chat_data[3],
                last_sender=chat_data[4],
                last_is_from_me=chat_data[5]
            )
            result.append(chat)
            
        return result
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        return []
    finally:
        if 'conn' in locals():
            conn.close()


def get_last_interaction(jid: str) -> str:
    """Get most recent message involving the contact."""
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        cursor.execute("""
            SELECT 
                m.timestamp,
                m.sender,
                c.name,
                m.content,
                m.is_from_me,
                c.jid,
                m.id,
                m.media_type
            FROM messages m
            JOIN chats c ON m.chat_jid = c.jid
            WHERE m.sender = ? OR c.jid = ?
            ORDER BY m.timestamp DESC
            LIMIT 1
        """, (jid, jid))
        
        msg_data = cursor.fetchone()
        
        if not msg_data:
            return None
            
        message = Message(
            timestamp=datetime.fromisoformat(msg_data[0]),
            sender=msg_data[1],
            chat_name=msg_data[2],
            content=msg_data[3],
            is_from_me=msg_data[4],
            chat_jid=msg_data[5],
            id=msg_data[6],
            media_type=msg_data[7]
        )
        
        return format_message(message)
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        return None
    finally:
        if 'conn' in locals():
            conn.close()


def get_chat(chat_jid: str, include_last_message: bool = True) -> Optional[Chat]:
    """Get chat metadata by JID."""
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        query = """
            SELECT 
                c.jid,
                c.name,
                c.last_message_time,
                m.content as last_message,
                m.sender as last_sender,
                m.is_from_me as last_is_from_me
            FROM chats c
        """
        
        if include_last_message:
            query += """
                LEFT JOIN messages m ON c.jid = m.chat_jid 
                AND c.last_message_time = m.timestamp
            """
            
        query += " WHERE c.jid = ?"
        
        cursor.execute(query, (chat_jid,))
        chat_data = cursor.fetchone()
        
        if not chat_data:
            return None
            
        return Chat(
            jid=chat_data[0],
            name=chat_data[1],
            last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
            last_message=chat_data[3],
            last_sender=chat_data[4],
            last_is_from_me=chat_data[5]
        )
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        return None
    finally:
        if 'conn' in locals():
            conn.close()


def get_direct_chat_by_contact(sender_phone_number: str) -> Optional[Chat]:
    """Get chat metadata by sender phone number."""
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        cursor.execute("""
            SELECT 
                c.jid,
                c.name,
                c.last_message_time,
                m.content as last_message,
                m.sender as last_sender,
                m.is_from_me as last_is_from_me
            FROM chats c
            LEFT JOIN messages m ON c.jid = m.chat_jid 
                AND c.last_message_time = m.timestamp
            WHERE c.jid LIKE ? AND c.jid NOT LIKE '%@g.us'
            LIMIT 1
        """, (f"%{sender_phone_number}%",))
        
        chat_data = cursor.fetchone()
        
        if not chat_data:
            return None
            
        return Chat(
            jid=chat_data[0],
            name=chat_data[1],
            last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
            last_message=chat_data[3],
            last_sender=chat_data[4],
            last_is_from_me=chat_data[5]
        )
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        return None
    finally:
        if 'conn' in locals():
            conn.close()


def get_own_identity() -> Dict[str, Any]:
    return _bridge_api_request("GET", "identity")


def list_joined_groups() -> Dict[str, Any]:
    return _bridge_api_request("GET", "groups")


def get_group_info(group_jid: str) -> Dict[str, Any]:
    return _bridge_api_request("GET", "groups/info", params={"jid": group_jid})


def create_group(name: str, participants: List[str]) -> Dict[str, Any]:
    return _bridge_api_request(
        "POST", "groups/create", {"name": name, "participants": participants}
    )


def update_group_participants(
    group_jid: str, participants: List[str], action: str
) -> Dict[str, Any]:
    return _bridge_api_request(
        "POST",
        "groups/participants",
        {"group_jid": group_jid, "participants": participants, "action": action},
    )


def set_chat_read_state(chat_jid: str, read: bool) -> Dict[str, Any]:
    return _bridge_api_request(
        "POST", "chat/read-state", {"chat_jid": chat_jid, "read": read}
    )


def is_on_whatsapp(phone_numbers: List[str]) -> Dict[str, Any]:
    return _bridge_api_request(
        "POST", "contacts/is-on-whatsapp", {"phone_numbers": phone_numbers}
    )


def set_chat_mute(
    chat_jid: str, muted: bool, duration_seconds: int = 0
) -> Dict[str, Any]:
    return _bridge_api_request(
        "POST",
        "chat/mute",
        {
            "chat_jid": chat_jid,
            "muted": muted,
            "duration_seconds": duration_seconds,
        },
    )


def set_chat_archive(chat_jid: str, archived: bool) -> Dict[str, Any]:
    return _bridge_api_request(
        "POST", "chat/archive", {"chat_jid": chat_jid, "archived": archived}
    )


def set_contact_block(jid: str, blocked: bool) -> Dict[str, Any]:
    return _bridge_api_request(
        "POST", "contact/block", {"jid": jid, "blocked": blocked}
    )


def send_typing_indicator(
    chat_jid: str, state: str, media: str = ""
) -> Dict[str, Any]:
    return _bridge_api_request(
        "POST",
        "chat/presence",
        {"chat_jid": chat_jid, "state": state, "media": media},
    )

def send_message(recipient: str, message: str) -> Tuple[bool, str]:
    try:
        # Validate input
        if not recipient:
            return False, "Recipient must be provided"
        
        url = f"{WHATSAPP_API_BASE_URL}/send"
        payload = {
            "recipient": recipient,
            "message": message,
        }
        
        response = requests.post(url, json=payload, timeout=BRIDGE_REQUEST_TIMEOUT_SECONDS)
        
        # Check if the request was successful
        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get("message", "Unknown response")
        else:
            return False, f"Error: HTTP {response.status_code} - {response.text}"
            
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, f"Error parsing response: {response.text}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"

def send_file(recipient: str, media_path: str) -> Tuple[bool, str]:
    try:
        # Validate input
        if not recipient:
            return False, "Recipient must be provided"
        
        if not media_path:
            return False, "Media path must be provided"
        
        if not os.path.isfile(media_path):
            return False, f"Media file not found: {media_path}"
        
        url = f"{WHATSAPP_API_BASE_URL}/send"
        payload = {
            "recipient": recipient,
            "media_path": media_path
        }
        
        response = requests.post(url, json=payload, timeout=MEDIA_REQUEST_TIMEOUT_SECONDS)
        
        # Check if the request was successful
        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get("message", "Unknown response")
        else:
            return False, f"Error: HTTP {response.status_code} - {response.text}"
            
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, f"Error parsing response: {response.text}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"

def send_audio_message(recipient: str, media_path: str) -> Tuple[bool, str]:
    try:
        # Validate input
        if not recipient:
            return False, "Recipient must be provided"
        
        if not media_path:
            return False, "Media path must be provided"
        
        if not os.path.isfile(media_path):
            return False, f"Media file not found: {media_path}"

        if not media_path.endswith(".ogg"):
            try:
                media_path = audio.convert_to_opus_ogg_temp(media_path)
            except Exception as e:
                return False, f"Error converting file to opus ogg. You likely need to install ffmpeg: {str(e)}"
        
        url = f"{WHATSAPP_API_BASE_URL}/send"
        payload = {
            "recipient": recipient,
            "media_path": media_path
        }
        
        response = requests.post(url, json=payload, timeout=MEDIA_REQUEST_TIMEOUT_SECONDS)
        
        # Check if the request was successful
        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get("message", "Unknown response")
        else:
            return False, f"Error: HTTP {response.status_code} - {response.text}"
            
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, f"Error parsing response: {response.text}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"

def download_media(message_id: str, chat_jid: str) -> Optional[str]:
    """Download media from a message and return the local file path.
    
    Args:
        message_id: The ID of the message containing the media
        chat_jid: The JID of the chat containing the message
    
    Returns:
        The local file path if download was successful, None otherwise
    """
    result = _bridge_api_request(
        "POST",
        "download",
        payload={"message_id": message_id, "chat_jid": chat_jid},
        timeout=MEDIA_REQUEST_TIMEOUT_SECONDS,
    )
    if result.get("success"):
        return result.get("path")
    return None
