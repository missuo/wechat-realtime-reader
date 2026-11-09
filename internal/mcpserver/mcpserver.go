// Package mcpserver exposes the read-only query layer as MCP tools.
package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/missuo/wechat-realtime-reader/internal/media"
	"github.com/missuo/wechat-realtime-reader/internal/snapshot"
	"github.com/missuo/wechat-realtime-reader/internal/store"
)

// Version is reported to MCP clients.
var Version = "0.1.0"

// Msg is the compact message form returned to agents.
type Msg struct {
	Time     string `json:"time"`
	Chat     string `json:"chat,omitempty"`
	ChatName string `json:"chat_name,omitempty"`
	Sender   string `json:"sender,omitempty"`
	From     string `json:"from"`
	Kind     string `json:"kind"`
	Text     string `json:"text"`
	Quote    string `json:"quote,omitempty"`
	URL      string `json:"url,omitempty"`
	MediaID  string `json:"media_id,omitempty"`
	Seq      int64  `json:"seq"`
}

func compact(ms []store.Message, withChat bool) []Msg {
	out := make([]Msg, 0, len(ms))
	for _, m := range ms {
		c := Msg{
			Time:   m.Time.Format("2006-01-02 15:04:05"),
			Sender: m.Sender, From: m.SenderName,
			Kind: m.Kind, Text: m.Text, Seq: m.Seq, MediaID: m.MediaID,
		}
		if withChat {
			c.Chat, c.ChatName = m.Chat, m.ChatName
		}
		if m.Extra != nil {
			if q := m.Extra.Quote; q != nil {
				c.Quote = strings.TrimSpace(q.SenderName + ": " + q.Text)
			}
			if m.Kind == "link" || m.Kind == "file" || m.Kind == "channels" {
				c.URL = m.Extra.URL
			}
			if m.Kind == "link" {
				c.Text = "[链接] " + m.Extra.Title
				if m.Extra.Desc != "" {
					c.Text += " — " + m.Extra.Desc
				}
			}
		}
		out = append(out, c)
	}
	return out
}

// New builds the MCP server. status may be nil when no syncer runs in
// this process.
func New(st *store.Store, lib *media.Library, status func() map[string]snapshot.DBStatus) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "wechat-realtime-reader", Version: Version}, &mcp.ServerOptions{
		Instructions: "Read-only access to the local WeChat (macOS 4.x) chat history, refreshed in near real time. " +
			"Chats and senders can be given as usernames (wxid_…, …@chatroom) or as display names/remarks; names are resolved fuzzily. " +
			"Times accept 2006-01-02, '2006-01-02 15:04', RFC3339, unix seconds or relative like 24h / 7d. " +
			"Every result carries freshness (fresh/stale) and lag_ms. Typical flow: list_sessions or search_contacts → get_messages; use get_new_messages with the returned cursor to follow new messages.",
	})

	type sessionsIn struct {
		Keyword string `json:"keyword,omitempty" jsonschema:"filter by contact/group name, remark, alias or pinyin"`
		Kind    string `json:"kind,omitempty" jsonschema:"friend | chatroom | official"`
		Since   string `json:"since,omitempty" jsonschema:"only sessions active since this time"`
		Limit   int    `json:"limit,omitempty" jsonschema:"max results, default 30"`
	}
	type sessionsOut struct {
		Sessions  []store.Session `json:"sessions"`
		Freshness store.Freshness `json:"freshness"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_sessions",
		Description: "List recent conversations (newest first) with unread count, last message summary and time.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sessionsIn) (*mcp.CallToolResult, sessionsOut, error) {
		since, err := store.ParseTime(in.Since)
		if err != nil {
			return nil, sessionsOut{}, err
		}
		if in.Limit <= 0 {
			in.Limit = 30
		}
		ss, err := st.Sessions(ctx, store.SessionQuery{Keyword: in.Keyword, Kind: in.Kind, Since: since, Limit: in.Limit})
		if err != nil {
			return nil, sessionsOut{}, err
		}
		if ss == nil {
			ss = []store.Session{}
		}
		return nil, sessionsOut{Sessions: ss, Freshness: st.FreshnessFor("session/session.db")}, nil
	})

	type contactsIn struct {
		Keyword string `json:"keyword" jsonschema:"name, remark, alias, wxid or pinyin to search"`
		Kind    string `json:"kind,omitempty" jsonschema:"friend | chatroom | official | stranger"`
		Limit   int    `json:"limit,omitempty" jsonschema:"max results, default 20"`
	}
	type contactsOut struct {
		Contacts []*store.Contact `json:"contacts"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_contacts",
		Description: "Find contacts, group chats and official accounts by name, remark, alias, wxid or pinyin. Use the returned username in other tools.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in contactsIn) (*mcp.CallToolResult, contactsOut, error) {
		if in.Limit <= 0 {
			in.Limit = 20
		}
		cs, err := st.SearchContacts(ctx, store.ContactQuery{Keyword: in.Keyword, Kind: in.Kind, Limit: in.Limit})
		if cs == nil {
			cs = []*store.Contact{}
		}
		return nil, contactsOut{Contacts: cs}, err
	})

	type messagesIn struct {
		Chat      string `json:"chat" jsonschema:"conversation: username (wxid_…, …@chatroom) or contact/group name"`
		Sender    string `json:"sender,omitempty" jsonschema:"only messages from this person (username, remark, nickname or group nickname); use 'me' for your own"`
		Keyword   string `json:"keyword,omitempty" jsonschema:"case-insensitive text filter"`
		Since     string `json:"since,omitempty" jsonschema:"start time (inclusive)"`
		Until     string `json:"until,omitempty" jsonschema:"end time (inclusive); bare date = end of day"`
		Limit     int    `json:"limit,omitempty" jsonschema:"max messages, default 50, max 1000"`
		BeforeSeq int64  `json:"before_seq,omitempty" jsonschema:"pagination: pass next_before from the previous page to get older messages"`
	}
	type messagesOut struct {
		Chat       string          `json:"chat"`
		ChatName   string          `json:"chat_name"`
		Sender     string          `json:"sender_filter,omitempty"`
		Messages   []Msg           `json:"messages"`
		HasMore    bool            `json:"has_more"`
		NextBefore int64           `json:"next_before,omitempty"`
		Freshness  store.Freshness `json:"freshness"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name: "get_messages",
		Description: "Read the message history of one conversation in chronological order (the newest `limit` messages in the range). " +
			"Optionally filter by sender (e.g. one member of a group), keyword and time range. Page backwards with before_seq.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in messagesIn) (*mcp.CallToolResult, messagesOut, error) {
		var out messagesOut
		chat, err := st.ResolveChat(ctx, in.Chat)
		if err != nil {
			return nil, out, err
		}
		q, err := buildQuery(ctx, st, chat.Username, in.Sender, in.Keyword, in.Since, in.Until, in.Limit)
		if err != nil {
			return nil, out, err
		}
		q.BeforeSeq, q.Asc = in.BeforeSeq, true
		page, err := st.Messages(ctx, q)
		if err != nil {
			return nil, out, err
		}
		out = messagesOut{Chat: chat.Username, ChatName: chat.DisplayName, Sender: q.Sender,
			Messages: compact(page.Messages, false), HasMore: page.HasMore, NextBefore: page.NextBefore, Freshness: page.Freshness}
		return nil, out, nil
	})

	type searchIn struct {
		Keyword string `json:"keyword,omitempty" jsonschema:"text to search for"`
		Sender  string `json:"sender,omitempty" jsonschema:"only messages sent by this person, across all chats (their private chat and every group)"`
		Chat    string `json:"chat,omitempty" jsonschema:"restrict to one conversation"`
		Since   string `json:"since,omitempty" jsonschema:"start time (inclusive); narrowing the range makes global search faster"`
		Until   string `json:"until,omitempty" jsonschema:"end time (inclusive)"`
		Limit   int    `json:"limit,omitempty" jsonschema:"max messages, default 50"`
	}
	type searchOut struct {
		Messages  []Msg           `json:"messages"`
		HasMore   bool            `json:"has_more"`
		Freshness store.Freshness `json:"freshness"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name: "search_messages",
		Description: "Search messages across conversations by keyword and/or sender (newest first). " +
			"With only sender set, returns everything that person said in any chat. At least one of keyword or sender is required.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, searchOut, error) {
		var out searchOut
		if strings.TrimSpace(in.Keyword) == "" && strings.TrimSpace(in.Sender) == "" {
			return nil, out, fmt.Errorf("keyword or sender is required")
		}
		chat := ""
		if in.Chat != "" {
			c, err := st.ResolveChat(ctx, in.Chat)
			if err != nil {
				return nil, out, err
			}
			chat = c.Username
		}
		q, err := buildQuery(ctx, st, chat, in.Sender, in.Keyword, in.Since, in.Until, in.Limit)
		if err != nil {
			return nil, out, err
		}
		page, err := st.SearchMessages(ctx, q)
		if err != nil {
			return nil, out, err
		}
		if chat != "" {
			name := st.DisplayName(ctx, chat)
			for i := range page.Messages {
				page.Messages[i].ChatName = name
			}
		}
		return nil, searchOut{Messages: compact(page.Messages, true), HasMore: page.HasMore, Freshness: page.Freshness}, nil
	})

	type membersIn struct {
		Chat string `json:"chat" jsonschema:"conversation username or name"`
	}
	type membersOut struct {
		Chat    string             `json:"chat"`
		Members []store.ChatMember `json:"members"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_chat_members",
		Description: "List the people who have sent messages in a conversation, most active first, with message counts. Useful to pick a sender filter.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in membersIn) (*mcp.CallToolResult, membersOut, error) {
		chat, err := st.ResolveChat(ctx, in.Chat)
		if err != nil {
			return nil, membersOut{}, err
		}
		ms, err := st.ChatMembers(ctx, chat.Username)
		if ms == nil {
			ms = []store.ChatMember{}
		}
		return nil, membersOut{Chat: chat.Username, Members: ms}, err
	})

	type newIn struct {
		SinceCursor int64  `json:"since_cursor,omitempty" jsonschema:"cursor returned by the previous call; omit to get the last 10 minutes"`
		Chat        string `json:"chat,omitempty" jsonschema:"only this conversation"`
		Limit       int    `json:"limit,omitempty" jsonschema:"max messages, default 200"`
	}
	type newOut struct {
		Messages  []Msg           `json:"messages"`
		Cursor    int64           `json:"cursor"`
		Freshness store.Freshness `json:"freshness"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_new_messages",
		Description: "Return messages that arrived after a cursor across all chats (oldest first). Store the returned cursor and pass it back to follow the stream.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in newIn) (*mcp.CallToolResult, newOut, error) {
		cursor := in.SinceCursor
		if cursor <= 0 {
			cursor = time.Now().Add(-10 * time.Minute).Unix()
		}
		chat := ""
		if in.Chat != "" {
			c, err := st.ResolveChat(ctx, in.Chat)
			if err != nil {
				return nil, newOut{}, err
			}
			chat = c.Username
		}
		res, err := st.NewMessages(ctx, cursor, chat, in.Limit)
		if err != nil {
			return nil, newOut{}, err
		}
		return nil, newOut{Messages: compact(res.Messages, true), Cursor: res.Cursor, Freshness: res.Freshness}, nil
	})

	if lib != nil {
		type imageIn struct {
			Chat    string `json:"chat" jsonschema:"conversation username or name the image was sent in"`
			MediaID string `json:"media_id" jsonschema:"media_id of an image message (kind image) from get_messages / search_messages"`
			Time    string `json:"time" jsonschema:"the message time as returned (e.g. 2026-11-04 08:32:30)"`
			Full    bool   `json:"full,omitempty" jsonschema:"return the original instead of the thumbnail"`
		}
		mcp.AddTool(s, &mcp.Tool{
			Name:        "get_image",
			Description: "Return the picture of an image message so you can look at it. Thumbnails are almost always available; originals only once WeChat has downloaded them (it skips many group-chat images until the user views them). When only the thumbnail exists, the result says THUMBNAIL ONLY.",
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in imageIn) (*mcp.CallToolResult, any, error) {
			chat, err := st.ResolveChat(ctx, in.Chat)
			if err != nil {
				return nil, nil, err
			}
			t, err := store.ParseTime(in.Time)
			if err != nil {
				return nil, nil, err
			}
			size := media.Thumb
			if in.Full {
				size = media.Full
			}
			b, mime, thumbOnly, err := lib.Image(ctx, chat.Username, []string{in.MediaID}, t, size)
			if err != nil {
				return nil, nil, err
			}
			content := []mcp.Content{&mcp.ImageContent{Data: b, MIMEType: mime}}
			switch {
			case thumbOnly:
				content = append(content, &mcp.TextContent{Text: "THUMBNAIL ONLY: the original image is not downloaded to this Mac, so this is WeChat's small low-resolution thumbnail. Small text and fine detail may be unreadable; do not guess at them. The original appears once the user opens the image (or scrolls to it) in WeChat."})
			case !in.Full:
				content = append(content, &mcp.TextContent{Text: "This is the thumbnail. Call again with full=true for the original if detail matters."})
			}
			return &mcp.CallToolResult{Content: content}, nil, nil
		})

		type videoIn struct {
			MediaID string `json:"media_id" jsonschema:"media_id of a video message (kind video) from get_messages / search_messages"`
			Time    string `json:"time" jsonschema:"the message time as returned (e.g. 2026-11-04 08:32:30)"`
		}
		mcp.AddTool(s, &mcp.Tool{
			Name:        "get_video",
			Description: "Return the cover frame of a video message so you can look at it, plus the local path of the mp4 (open it, or run ffmpeg on it for more frames / audio). Only videos already downloaded by WeChat on this Mac are available.",
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in videoIn) (*mcp.CallToolResult, any, error) {
			t, err := store.ParseTime(in.Time)
			if err != nil {
				return nil, nil, err
			}
			var content []mcp.Content
			if b, mime, err := lib.VideoCover(in.MediaID, t); err == nil {
				content = append(content, &mcp.ImageContent{Data: b, MIMEType: mime})
			}
			p, perr := lib.VideoPath(in.MediaID, t)
			switch {
			case perr == nil:
				content = append(content, &mcp.TextContent{Text: "mp4: " + p})
			case len(content) == 0:
				return nil, nil, perr
			default:
				content = append(content, &mcp.TextContent{Text: "mp4 not downloaded to this Mac (only the cover is available); open it once in WeChat"})
			}
			return &mcp.CallToolResult{Content: content}, nil, nil
		})
	}

	type statusOut struct {
		DBs map[string]snapshot.DBStatus `json:"dbs"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "sync_status",
		Description: "Show snapshot freshness per database: status, source commit time, snapshot time and lag.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, statusOut, error) {
		out := statusOut{DBs: map[string]snapshot.DBStatus{}}
		if status != nil {
			out.DBs = status()
		}
		return nil, out, nil
	})
	return s
}

func buildQuery(ctx context.Context, st *store.Store, chat, sender, keyword, since, until string, limit int) (store.MessageQuery, error) {
	q := store.MessageQuery{Chat: chat, Keyword: keyword, Limit: limit}
	var err error
	if q.Since, err = store.ParseTime(since); err != nil {
		return q, err
	}
	if q.Until, err = store.ParseUntil(until); err != nil {
		return q, err
	}
	if sender != "" {
		if q.Sender, err = st.ResolveSender(ctx, chat, sender); err != nil {
			return q, err
		}
	}
	return q, nil
}
