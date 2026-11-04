package store

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Message is a parsed chat message.
type Message struct {
	Chat       string    `json:"chat"`
	ChatName   string    `json:"chat_name,omitempty"`
	LocalID    int64     `json:"local_id"`
	ServerID   string    `json:"server_id"`
	Seq        int64     `json:"seq"` // sort_seq, monotonic within a chat; pagination cursor
	Time       time.Time `json:"time"`
	Type       int       `json:"type"`
	SubType    int       `json:"sub_type,omitempty"`
	Kind       string    `json:"kind"`
	Sender     string    `json:"sender"`
	SenderName string    `json:"sender_name"`
	IsSelf     bool      `json:"is_self"`
	Text       string    `json:"text"`
	Raw        string    `json:"raw,omitempty"`
	Extra      *Extra    `json:"extra,omitempty"`
	// MediaID names the local attachment files for images
	// (<media_id>[_t|_h].dat under msg/attach) and videos
	// (<media_id>.mp4 / _thumb.jpg under msg/video).
	MediaID string `json:"media_id,omitempty"`
	DB      string `json:"-"`
}

// MessageQuery selects messages.
type MessageQuery struct {
	Chat      string    // username (required for Messages)
	Sender    string    // only messages sent by this username
	Keyword   string    // case-insensitive substring of the rendered text
	Since     time.Time // inclusive
	Until     time.Time // inclusive
	BeforeSeq int64     // pagination: only seq < BeforeSeq
	AfterSeq  int64     // only seq > AfterSeq (new-message polling)
	Types     []int     // base local_type filter (1 text, 3 image, 49 app, ...)
	Limit     int
	Asc       bool // return oldest first (default newest first)
	WithRaw   bool // include raw XML for non-text messages
}

// MessagePage is a page of results with a cursor.
type MessagePage struct {
	Messages   []Message `json:"messages"`
	HasMore    bool      `json:"has_more"`
	NextBefore int64     `json:"next_before,omitempty"`
	Freshness  Freshness `json:"freshness"`
}

const maxLimit = 1000

func (q *MessageQuery) norm() {
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Limit > maxLimit {
		q.Limit = maxLimit
	}
	q.Keyword = strings.ToLower(strings.TrimSpace(q.Keyword))
}

// Messages returns messages of one chat across all message shards.
func (s *Store) Messages(ctx context.Context, q MessageQuery) (*MessagePage, error) {
	q.norm()
	if q.Chat == "" {
		return nil, fmt.Errorf("chat is required")
	}
	cs, err := s.contactsSet(ctx)
	if err != nil {
		return nil, err
	}
	table := TableName(q.Chat)
	var all []Message
	var dbs []string
	for _, rel := range s.messageDBs() {
		h, err := s.get(rel)
		if err != nil {
			return nil, err
		}
		if !h.tables[table] {
			continue
		}
		dbs = append(dbs, rel)
		msgs, err := s.scanTable(ctx, h, rel, q.Chat, table, q, cs, q.Limit+1)
		if err != nil {
			return nil, err
		}
		all = append(all, msgs...)
	}
	page := s.finish(all, q)
	page.Freshness = s.FreshnessFor(append(dbs, sessionDB)...)
	return page, nil
}

func (s *Store) finish(all []Message, q MessageQuery) *MessagePage {
	sort.Slice(all, func(i, j int) bool {
		if all[i].Time.Equal(all[j].Time) {
			return all[i].Seq > all[j].Seq
		}
		return all[i].Time.After(all[j].Time)
	})
	page := &MessagePage{}
	if len(all) > q.Limit {
		all = all[:q.Limit]
		page.HasMore = true
	}
	if page.HasMore && len(all) > 0 {
		page.NextBefore = all[len(all)-1].Seq
	}
	if q.Asc {
		for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
			all[i], all[j] = all[j], all[i]
		}
	}
	if all == nil {
		all = []Message{}
	}
	page.Messages = all
	return page
}

// scanTable reads up to want matching messages from one table, newest first.
func (s *Store) scanTable(ctx context.Context, h *handle, rel, chat, table string, q MessageQuery, cs *contactSet, want int) ([]Message, error) {
	var where []string
	var args []any
	if q.Sender != "" {
		id, ok := h.ids[q.Sender]
		if !ok {
			return nil, nil
		}
		where = append(where, "real_sender_id = ?")
		args = append(args, id)
	}
	if !q.Since.IsZero() {
		where = append(where, "create_time >= ?")
		args = append(args, q.Since.Unix())
	}
	if !q.Until.IsZero() {
		where = append(where, "create_time <= ?")
		args = append(args, q.Until.Unix())
	}
	if q.BeforeSeq > 0 {
		where = append(where, "sort_seq < ?")
		args = append(args, q.BeforeSeq)
	}
	if q.AfterSeq > 0 {
		where = append(where, "sort_seq > ?")
		args = append(args, q.AfterSeq)
	}
	if len(q.Types) > 0 {
		ph := strings.Repeat("?,", len(q.Types))
		where = append(where, "(local_type & 4294967295) IN ("+ph[:len(ph)-1]+")")
		for _, t := range q.Types {
			args = append(args, t)
		}
	}
	if q.Keyword != "" {
		// Compressed rows cannot be filtered in SQL; uncompressed ones can.
		where = append(where, "(IFNULL(WCDB_CT_message_content,0) != 0 OR message_content LIKE ?)")
		// LIKE wildcards in the keyword only widen this pre-filter; the exact
		// check runs on the rendered text below.
		args = append(args, "%"+q.Keyword+"%")
	}
	sqlq := `SELECT local_id, IFNULL(server_id,0), local_type, IFNULL(sort_seq,0), IFNULL(real_sender_id,0),
		create_time, IFNULL(status,0), message_content, WCDB_CT_message_content, packed_info_data FROM ` + table
	if len(where) > 0 {
		sqlq += " WHERE " + strings.Join(where, " AND ")
	}
	sqlq += " ORDER BY sort_seq DESC"
	if q.Keyword == "" {
		sqlq += " LIMIT " + strconv.Itoa(want)
	}
	rows, err := h.db.QueryContext(ctx, sqlq, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	isRoom := strings.HasSuffix(chat, "@chatroom")
	var out []Message
	for rows.Next() {
		var (
			m        Message
			serverID int64
			ltype    int64
			senderID int64
			ctime    int64
			status   int64
			content  []byte
			ct       sql.NullInt64
			packed   []byte
		)
		if err := rows.Scan(&m.LocalID, &serverID, &ltype, &m.Seq, &senderID, &ctime, &status, &content, &ct, &packed); err != nil {
			return nil, err
		}
		m.Chat, m.DB = chat, rel
		m.ServerID = strconv.FormatInt(serverID, 10)
		m.Time = time.Unix(ctime, 0)
		m.Type, m.SubType = int(ltype&0xffffffff), int(ltype>>32)
		m.Sender = h.names[senderID]
		body := s.decompress(content, ct)
		if isRoom {
			if p, rest, ok := splitSenderPrefix(body); ok {
				if m.Sender == "" {
					m.Sender = p
				}
				body = rest
			}
		}
		m.IsSelf = m.Sender == s.self
		if m.Sender == "" && !isRoom && m.Type != 10000 && m.Type != 10002 {
			m.Sender = chat
		}
		switch {
		case m.Sender == "":
			m.SenderName = "系统"
		case m.IsSelf:
			m.SenderName = "我"
		default:
			m.SenderName = cs.senderName(chat, m.Sender)
		}
		render(&m, body, cs)
		if m.Type == 3 || m.Type == 43 {
			if id := mediaIDRe.Find(packed); id != nil {
				m.MediaID = string(id)
			}
		}
		if q.WithRaw && m.Type != 1 {
			m.Raw = body
		}
		if q.Keyword != "" && !strings.Contains(strings.ToLower(m.Text), q.Keyword) {
			continue
		}
		out = append(out, m)
		if len(out) >= want {
			break
		}
	}
	return out, rows.Err()
}

var mediaIDRe = regexp.MustCompile(`[0-9a-f]{32}`)

var senderPrefix = regexp.MustCompile(`^([A-Za-z0-9_\-@.]{3,64}):\n`)

func splitSenderPrefix(s string) (string, string, bool) {
	if m := senderPrefix.FindStringSubmatchIndex(s); m != nil {
		return s[m[2]:m[3]], s[m[1]:], true
	}
	return "", s, false
}

func stripSenderPrefix(s string) string {
	_, rest, _ := splitSenderPrefix(s)
	return rest
}

// SearchMessages searches across chats. With Chat set it behaves like
// Messages. With Sender set (and no Chat) it searches the private chat with
// that sender and every group chat. Otherwise it scans every chat, newest
// shard first, until Limit matches are found.
func (s *Store) SearchMessages(ctx context.Context, q MessageQuery) (*MessagePage, error) {
	if q.Chat != "" {
		return s.Messages(ctx, q)
	}
	q.norm()
	cs, err := s.contactsSet(ctx)
	if err != nil {
		return nil, err
	}
	var all []Message
	var dbs []string
	deadline := time.Now().Add(20 * time.Second)
	for _, rel := range s.messageDBs() {
		h, err := s.get(rel)
		if err != nil {
			return nil, err
		}
		dbs = append(dbs, rel)
		if q.Sender != "" {
			if _, ok := h.ids[q.Sender]; !ok {
				continue
			}
		}
		for _, chat := range s.chatsIn(h, q.Sender) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if time.Now().After(deadline) {
				break
			}
			msgs, err := s.scanTable(ctx, h, rel, chat, TableName(chat), q, cs, q.Limit+1)
			if err != nil {
				return nil, err
			}
			all = append(all, msgs...)
		}
	}
	for i := range all {
		all[i].ChatName = cs.displayName(all[i].Chat)
	}
	page := s.finish(all, q)
	page.Freshness = s.FreshnessFor(dbs...)
	return page, nil
}

// chatsIn lists chats with a message table in this shard. When sender is
// set, only chats where that sender can appear: their private chat, your
// private chats (if the sender is you) and all groups.
func (s *Store) chatsIn(h *handle, sender string) []string {
	var out []string
	for name := range h.ids {
		t := TableName(name)
		if !h.tables[t] {
			continue
		}
		if sender != "" && sender != s.self && name != sender && !strings.HasSuffix(name, "@chatroom") {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (cs *contactSet) displayName(u string) string {
	if c := cs.byName[u]; c != nil {
		return c.DisplayName
	}
	return u
}

// ChatMember is a sender that appears in a chat, for filtering.
type ChatMember struct {
	Username string    `json:"username"`
	Name     string    `json:"name"`
	Count    int       `json:"count"`
	LastTime time.Time `json:"last_time"`
	IsSelf   bool      `json:"is_self,omitempty"`
}

// ChatMembers lists who has spoken in a chat, most active first.
func (s *Store) ChatMembers(ctx context.Context, chat string) ([]ChatMember, error) {
	cs, err := s.contactsSet(ctx)
	if err != nil {
		return nil, err
	}
	table := TableName(chat)
	agg := map[string]*ChatMember{}
	for _, rel := range s.messageDBs() {
		h, err := s.get(rel)
		if err != nil {
			return nil, err
		}
		if !h.tables[table] {
			continue
		}
		rows, err := h.db.QueryContext(ctx, `SELECT real_sender_id, COUNT(*), MAX(create_time) FROM `+table+
			` WHERE (local_type & 4294967295) NOT IN (10000, 10002) GROUP BY real_sender_id`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, n, ts int64
			if err := rows.Scan(&id, &n, &ts); err != nil {
				rows.Close()
				return nil, err
			}
			u := h.names[id]
			if u == "" || u == chat && strings.HasSuffix(chat, "@chatroom") {
				continue
			}
			m := agg[u]
			if m == nil {
				m = &ChatMember{Username: u, IsSelf: u == s.self}
				if m.IsSelf {
					m.Name = "我"
				} else {
					m.Name = cs.senderName(chat, u)
				}
				agg[u] = m
			}
			m.Count += int(n)
			if t := time.Unix(ts, 0); t.After(m.LastTime) {
				m.LastTime = t
			}
		}
		rows.Close()
	}
	out := make([]ChatMember, 0, len(agg))
	for _, m := range agg {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out, nil
}

// NewMessagesResult is returned by NewMessages.
type NewMessagesResult struct {
	Messages  []Message `json:"messages"`
	Cursor    int64     `json:"cursor"` // pass back as since_cursor
	Freshness Freshness `json:"freshness"`
}

// NewMessages returns messages created after cursor (unix seconds, or
// a value previously returned), across all chats whose session changed.
func (s *Store) NewMessages(ctx context.Context, cursor int64, chatFilter string, limit int) (*NewMessagesResult, error) {
	if limit <= 0 {
		limit = 200
	}
	since := time.Unix(cursor, 0)
	sess, err := s.Sessions(ctx, SessionQuery{Since: since, Limit: 500})
	if err != nil {
		return nil, err
	}
	res := &NewMessagesResult{Cursor: cursor}
	for _, ss := range sess {
		if chatFilter != "" && ss.Username != chatFilter {
			continue
		}
		page, err := s.Messages(ctx, MessageQuery{Chat: ss.Username, Since: time.Unix(cursor+1, 0), Limit: limit, Asc: true})
		if err != nil {
			return nil, err
		}
		for i := range page.Messages {
			page.Messages[i].ChatName = ss.Name
		}
		res.Messages = append(res.Messages, page.Messages...)
	}
	sort.SliceStable(res.Messages, func(i, j int) bool { return res.Messages[i].Time.Before(res.Messages[j].Time) })
	if len(res.Messages) > limit {
		res.Messages = res.Messages[len(res.Messages)-limit:]
	}
	for _, m := range res.Messages {
		res.Cursor = max(res.Cursor, m.Time.Unix())
	}
	if res.Messages == nil {
		res.Messages = []Message{}
	}
	res.Freshness = s.FreshnessFor()
	return res, nil
}
