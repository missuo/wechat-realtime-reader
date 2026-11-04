package store

import (
	"context"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const contactDB = "contact/contact.db"
const sessionDB = "session/session.db"
const headDB = "head_image/head_image.db"

// Contact is a person, group or official account.
type Contact struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Remark      string `json:"remark,omitempty"`
	NickName    string `json:"nick_name,omitempty"`
	Alias       string `json:"alias,omitempty"`
	Kind        string `json:"kind"` // friend | chatroom | official | stranger
	pinyin      string
}

type contactSet struct {
	self    string
	sig     sigT
	byName  map[string]*Contact
	list    []*Contact
	members map[string]map[string]string // chatroom → member username → group nickname
	roomsOf map[string][]string          // member username → chatrooms
}

func kindOf(username string, verifyFlag int64, fromStranger bool) string {
	switch {
	case strings.HasSuffix(username, "@chatroom"):
		return "chatroom"
	case strings.HasPrefix(username, "gh_") || verifyFlag != 0:
		return "official"
	case fromStranger:
		return "stranger"
	}
	return "friend"
}

func (s *Store) contactsSet(ctx context.Context) (*contactSet, error) {
	h, err := s.get(contactDB)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	cs := s.contacts
	s.mu.Unlock()
	if cs != nil && cs.sig == h.sig {
		return cs, nil
	}
	cs = &contactSet{self: s.self, sig: h.sig, byName: map[string]*Contact{}, members: map[string]map[string]string{}, roomsOf: map[string][]string{}}
	for _, tbl := range []string{"contact", "stranger"} {
		rows, err := h.db.QueryContext(ctx, `SELECT username, IFNULL(alias,''), IFNULL(remark,''), IFNULL(nick_name,''),
			IFNULL(verify_flag,0), IFNULL(quan_pin,''), IFNULL(remark_quan_pin,''), IFNULL(delete_flag,0) FROM `+tbl)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var c Contact
			var vf, del int64
			var qp, rqp string
			if err := rows.Scan(&c.Username, &c.Alias, &c.Remark, &c.NickName, &vf, &qp, &rqp, &del); err != nil {
				rows.Close()
				return nil, err
			}
			if c.Username == "" {
				continue
			}
			if _, dup := cs.byName[c.Username]; dup {
				continue // contact table wins over stranger
			}
			c.Kind = kindOf(c.Username, vf, tbl == "stranger")
			c.DisplayName = firstNonEmpty(c.Remark, c.NickName, c.Alias, c.Username)
			c.pinyin = strings.ToLower(qp + " " + rqp)
			cc := c
			cs.byName[c.Username] = &cc
			cs.list = append(cs.list, &cc)
		}
		rows.Close()
	}

	// Group member nicknames live in chat_room.ext_buffer (protobuf).
	rows, err := h.db.QueryContext(ctx, `SELECT username, ext_buffer FROM chat_room`)
	if err == nil {
		for rows.Next() {
			var room string
			var buf []byte
			if rows.Scan(&room, &buf) != nil {
				continue
			}
			m := parseRoomMembers(buf)
			cs.members[room] = m
			for u := range m {
				cs.roomsOf[u] = append(cs.roomsOf[u], room)
			}
		}
		rows.Close()
	}
	sort.Slice(cs.list, func(i, j int) bool { return cs.list[i].DisplayName < cs.list[j].DisplayName })

	s.mu.Lock()
	s.contacts = cs
	s.mu.Unlock()
	return cs, nil
}

// parseRoomMembers decodes repeated field 1 {1: username, 2: group nickname}.
func parseRoomMembers(b []byte) map[string]string {
	out := map[string]string{}
	pbEach(b, func(f int, wt int, v []byte) {
		if f != 1 || wt != 2 {
			return
		}
		var user, nick string
		pbEach(v, func(f int, wt int, v []byte) {
			if wt != 2 || !utf8.Valid(v) {
				return
			}
			switch f {
			case 1:
				user = string(v)
			case 2:
				nick = string(v)
			}
		})
		if user != "" {
			out[user] = nick
		}
	})
	return out
}

// pbEach walks one level of a protobuf message. Malformed input stops the
// walk silently.
func pbEach(b []byte, fn func(field, wireType int, val []byte)) {
	uvarint := func() (uint64, bool) {
		var x uint64
		for s := uint(0); s < 64; s += 7 {
			if len(b) == 0 {
				return 0, false
			}
			c := b[0]
			b = b[1:]
			x |= uint64(c&0x7f) << s
			if c < 0x80 {
				return x, true
			}
		}
		return 0, false
	}
	for len(b) > 0 {
		key, ok := uvarint()
		if !ok {
			return
		}
		f, wt := int(key>>3), int(key&7)
		switch wt {
		case 0:
			if _, ok := uvarint(); !ok {
				return
			}
		case 1:
			if len(b) < 8 {
				return
			}
			b = b[8:]
		case 2:
			l, ok := uvarint()
			if !ok || uint64(len(b)) < l {
				return
			}
			fn(f, wt, b[:l])
			b = b[l:]
		case 5:
			if len(b) < 4 {
				return
			}
			b = b[4:]
		default:
			return
		}
	}
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// Contact looks up one username (nil when unknown).
func (s *Store) Contact(ctx context.Context, username string) *Contact {
	cs, err := s.contactsSet(ctx)
	if err != nil {
		return nil
	}
	return cs.byName[username]
}

// DisplayName returns the best human name for a username.
func (s *Store) DisplayName(ctx context.Context, username string) string {
	if c := s.Contact(ctx, username); c != nil {
		return c.DisplayName
	}
	return username
}

// senderName prefers the group nickname inside a chatroom, then the
// contact name.
func (cs *contactSet) senderName(room, user string) string {
	if c := cs.byName[user]; c != nil && c.Remark != "" {
		return c.Remark
	}
	if m := cs.members[room]; m != nil && m[user] != "" {
		return m[user]
	}
	if c := cs.byName[user]; c != nil {
		return c.DisplayName
	}
	return user
}

// ContactQuery filters contacts.
type ContactQuery struct {
	Keyword string // matches remark, nickname, alias, username, pinyin
	Kind    string // friend | chatroom | official | stranger | "" (all but stranger)
	Limit   int
}

// SearchContacts finds contacts by name.
func (s *Store) SearchContacts(ctx context.Context, q ContactQuery) ([]*Contact, error) {
	cs, err := s.contactsSet(ctx)
	if err != nil {
		return nil, err
	}
	kw := strings.ToLower(strings.TrimSpace(q.Keyword))
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	type scored struct {
		c     *Contact
		score int
	}
	var res []scored
	for _, c := range cs.list {
		if q.Kind != "" && c.Kind != q.Kind {
			continue
		}
		if q.Kind == "" && c.Kind == "stranger" && kw == "" {
			continue
		}
		score := 0
		if kw != "" {
			score = matchScore(kw, c)
			if score == 0 {
				continue
			}
		}
		res = append(res, scored{c, score})
	}
	sort.SliceStable(res, func(i, j int) bool { return res[i].score > res[j].score })
	out := make([]*Contact, 0, min(limit, len(res)))
	for i := 0; i < len(res) && i < limit; i++ {
		out = append(out, res[i].c)
	}
	return out, nil
}

func matchScore(kw string, c *Contact) int {
	best := 0
	for i, f := range []string{c.Remark, c.NickName, c.Alias, c.Username} {
		lf := strings.ToLower(f)
		switch {
		case lf == "":
		case lf == kw:
			best = max(best, 100-i)
		case strings.HasPrefix(lf, kw):
			best = max(best, 80-i)
		case strings.Contains(lf, kw):
			best = max(best, 60-i)
		}
	}
	if best == 0 && strings.Contains(strings.ReplaceAll(c.pinyin, " ", ""), strings.ReplaceAll(kw, " ", "")) {
		best = 30
	}
	// Strangers rank below known contacts.
	if c.Kind == "stranger" && best > 0 {
		best -= 20
		if best <= 0 {
			best = 1
		}
	}
	return best
}

// ResolveChat maps a username or a fuzzy name to a username. Exact
// usernames win; otherwise the best-scoring non-stranger contact.
func (s *Store) ResolveChat(ctx context.Context, nameOrID string) (*Contact, error) {
	cs, err := s.contactsSet(ctx)
	if err != nil {
		return nil, err
	}
	if c := cs.byName[nameOrID]; c != nil {
		return c, nil
	}
	if strings.HasSuffix(nameOrID, "@chatroom") || strings.HasPrefix(nameOrID, "wxid_") || strings.HasPrefix(nameOrID, "gh_") {
		return &Contact{Username: nameOrID, DisplayName: nameOrID, Kind: kindOf(nameOrID, 0, false)}, nil
	}
	res, err := s.SearchContacts(ctx, ContactQuery{Keyword: nameOrID, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, &NotFoundError{What: "contact", Query: nameOrID}
	}
	return res[0], nil
}

// NotFoundError reports an unresolvable name.
type NotFoundError struct{ What, Query string }

func (e *NotFoundError) Error() string { return e.What + " not found: " + e.Query }

// Session is one entry of the conversation list.
type Session struct {
	Username       string    `json:"username"`
	Name           string    `json:"name"`
	Kind           string    `json:"kind"`
	Unread         int       `json:"unread"`
	Summary        string    `json:"summary"`
	LastTime       time.Time `json:"last_time"`
	LastSender     string    `json:"last_sender,omitempty"`
	LastSenderName string    `json:"last_sender_name,omitempty"`
}

// SessionQuery filters the conversation list.
type SessionQuery struct {
	Keyword string
	Kind    string
	Since   time.Time
	Limit   int
}

// Sessions returns recent conversations, newest first.
func (s *Store) Sessions(ctx context.Context, q SessionQuery) ([]Session, error) {
	h, err := s.get(sessionDB)
	if err != nil {
		return nil, err
	}
	cs, err := s.contactsSet(ctx)
	if err != nil {
		return nil, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	rows, err := h.db.QueryContext(ctx, `SELECT username, IFNULL(unread_count,0), IFNULL(summary,''), IFNULL(last_timestamp,0),
		IFNULL(last_msg_sender,''), IFNULL(last_sender_display_name,'')
		FROM SessionTable WHERE IFNULL(is_hidden,0)=0 AND last_timestamp >= ? ORDER BY sort_timestamp DESC`, q.Since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	kw := strings.ToLower(strings.TrimSpace(q.Keyword))
	var out []Session
	for rows.Next() {
		var ss Session
		var ts int64
		if err := rows.Scan(&ss.Username, &ss.Unread, &ss.Summary, &ts, &ss.LastSender, &ss.LastSenderName); err != nil {
			return nil, err
		}
		if isFolderSession(ss.Username) {
			continue
		}
		ss.LastTime = time.Unix(ts, 0)
		c := cs.byName[ss.Username]
		if c != nil {
			ss.Name, ss.Kind = c.DisplayName, c.Kind
		} else {
			ss.Name, ss.Kind = ss.Username, kindOf(ss.Username, 0, false)
		}
		if q.Kind != "" && ss.Kind != q.Kind {
			continue
		}
		if kw != "" && (c == nil || matchScore(kw, c) == 0) && !strings.Contains(strings.ToLower(ss.Name), kw) {
			continue
		}
		if ss.LastSender != "" && ss.LastSenderName == "" {
			ss.LastSenderName = cs.senderName(ss.Username, ss.LastSender)
		}
		ss.Summary = stripSenderPrefix(ss.Summary)
		out = append(out, ss)
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

// Avatar returns the cached avatar image for a username.
func (s *Store) Avatar(ctx context.Context, username string) ([]byte, error) {
	h, err := s.get(headDB)
	if err != nil {
		return nil, err
	}
	var b []byte
	err = h.db.QueryRowContext(ctx, `SELECT image_buffer FROM head_image WHERE username=?`, username).Scan(&b)
	return b, err
}

// isFolderSession reports pseudo sessions that group other chats.
func isFolderSession(u string) bool {
	return strings.HasPrefix(u, "@") || strings.HasSuffix(u, "sessionholder") || u == "notifymessage"
}

// ResolveSender maps a username or name to a sender username. Inside a chat
// it also matches group nicknames of people who have spoken there.
func (s *Store) ResolveSender(ctx context.Context, chat, q string) (string, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return "", nil
	}
	if q == "我" || q == "me" || q == "self" {
		return s.self, nil
	}
	cs, err := s.contactsSet(ctx)
	if err != nil {
		return "", err
	}
	if cs.byName[q] != nil || strings.HasPrefix(q, "wxid_") {
		return q, nil
	}
	if chat != "" {
		members, err := s.ChatMembers(ctx, chat)
		if err != nil {
			return "", err
		}
		lq := strings.ToLower(q)
		best, bestScore := "", 0
		for _, m := range members {
			score := 0
			c := cs.byName[m.Username]
			if c != nil {
				score = matchScore(lq, c)
			}
			if gn := strings.ToLower(cs.members[chat][m.Username]); gn != "" {
				switch {
				case gn == lq:
					score = max(score, 100)
				case strings.Contains(gn, lq):
					score = max(score, 70)
				}
			}
			if strings.Contains(strings.ToLower(m.Name), lq) {
				score = max(score, 50)
			}
			if score > bestScore {
				best, bestScore = m.Username, score
			}
		}
		if best != "" {
			return best, nil
		}
	}
	c, err := s.ResolveChat(ctx, q)
	if err != nil {
		return "", err
	}
	return c.Username, nil
}
