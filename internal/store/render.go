package store

import (
	"encoding/xml"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
)

// Extra carries structured fields for rich messages.
type Extra struct {
	Title    string `json:"title,omitempty"`
	Desc     string `json:"desc,omitempty"`
	URL      string `json:"url,omitempty"`
	Source   string `json:"source,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
	Duration int    `json:"duration_sec,omitempty"`
	Quote    *Quote `json:"quote,omitempty"`
	// Transcript is WeChat's speech-to-text result for a voice message.
	Transcript string `json:"transcript,omitempty"`
}

// Quote is the message a reply refers to.
type Quote struct {
	Sender     string `json:"sender,omitempty"`
	SenderName string `json:"sender_name,omitempty"`
	Text       string `json:"text"`
	ServerID   string `json:"server_id,omitempty"`
}

var kindNames = map[int]string{
	1: "text", 3: "image", 34: "voice", 37: "friend_request", 42: "card", 43: "video",
	47: "sticker", 48: "location", 49: "app", 50: "call", 10000: "system", 10002: "system",
}

var appKinds = map[int]string{
	3: "music", 4: "link", 5: "link", 6: "file", 8: "sticker", 17: "live_location", 19: "chat_record",
	24: "note", 33: "mini_program", 36: "mini_program", 40: "chat_record", 51: "channels", 53: "solitaire",
	57: "reply", 62: "pat", 63: "live", 74: "file", 87: "announcement", 2000: "transfer", 2001: "red_packet",
}

type appMsgXML struct {
	FromUser string `xml:"fromusername"`
	AppMsg   struct {
		Title  string `xml:"title"`
		Des    string `xml:"des"`
		Type   int    `xml:"type"`
		URL    string `xml:"url"`
		Source string `xml:"sourcedisplayname"`
		Attach struct {
			TotalLen int64 `xml:"totallen"`
		} `xml:"appattach"`
		Refer *struct {
			Type        int    `xml:"type"`
			SvrID       string `xml:"svrid"`
			FromUsr     string `xml:"fromusr"`
			ChatUsr     string `xml:"chatusr"`
			DisplayName string `xml:"displayname"`
			Content     string `xml:"content"`
		} `xml:"refermsg"`
		PayInfo struct {
			FeeDesc     string `xml:"feedesc"`
			PaySubType  int    `xml:"paysubtype"`
			PayMemo     string `xml:"pay_memo"`
			SenderTitle string `xml:"sendertitle"`
		} `xml:"wcpayinfo"`
		Pat struct {
			From     string `xml:"fromusername"`
			Template string `xml:"template"`
		} `xml:"patinfo"`
		Finder struct {
			Nickname string `xml:"nickname"`
			Desc     string `xml:"desc"`
		} `xml:"finderFeed"`
	} `xml:"appmsg"`
}

func parseXML(body string, v any) bool {
	i := strings.Index(body, "<")
	if i < 0 {
		return false
	}
	d := xml.NewDecoder(strings.NewReader(body[i:]))
	d.Strict = false
	d.AutoClose = xml.HTMLAutoClose
	d.Entity = xml.HTMLEntity
	return d.Decode(v) == nil
}

var attrRe = func(tag, attr string) *regexp.Regexp {
	return regexp.MustCompile(`<` + tag + `\b[^>]*?\s` + attr + `\s*=\s*"([^"]*)"`)
}

var (
	voiceLenRe  = attrRe("voicemsg", "voicelength")
	videoLenRe  = attrRe("videomsg", "playlength")
	locLabelRe  = attrRe("location", "label")
	locPoiRe    = attrRe("location", "poiname")
	cardNickRe  = attrRe("msg", "nickname")
	cardUserRe  = attrRe("msg", "username")
	replaceRe   = regexp.MustCompile(`(?s)<replacemsg>\s*(?:<!\[CDATA\[)?(.*?)(?:\]\]>)?\s*</replacemsg>`)
	templateRe  = regexp.MustCompile(`(?s)<template>\s*(?:<!\[CDATA\[)?(.*?)(?:\]\]>)?\s*</template>`)
	tagRe       = regexp.MustCompile(`<[^>]+>`)
	placeholder = regexp.MustCompile(`\$\{([^}]+)\}`)
)

func attr(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return html.UnescapeString(m[1])
	}
	return ""
}

// render fills Kind, Text and Extra from the message body.
func render(m *Message, body string, cs *contactSet) {
	m.Kind = kindNames[m.Type]
	if m.Kind == "" {
		m.Kind = "other"
	}
	switch m.Type {
	case 1:
		m.Text = body
	case 3:
		m.Text = "[图片]"
	case 34:
		m.Text = "[语音]"
		if ms, _ := strconv.Atoi(attr(voiceLenRe, body)); ms > 0 {
			sec := (ms + 999) / 1000
			m.Extra = &Extra{Duration: sec}
			m.Text = fmt.Sprintf("[语音 %d″]", sec)
		}
	case 43:
		m.Text = "[视频]"
		if sec, _ := strconv.Atoi(attr(videoLenRe, body)); sec > 0 {
			m.Extra = &Extra{Duration: sec}
			m.Text = fmt.Sprintf("[视频 %d:%02d]", sec/60, sec%60)
		}
	case 47:
		m.Text = "[表情]"
	case 48:
		label := firstNonEmpty(attr(locPoiRe, body), attr(locLabelRe, body))
		m.Text = strings.TrimSpace("[位置] " + label)
		if label != "" {
			m.Extra = &Extra{Title: label}
		}
	case 42:
		nick := attr(cardNickRe, body)
		m.Text = strings.TrimSpace("[名片] " + nick)
		m.Extra = &Extra{Title: nick, Source: attr(cardUserRe, body)}
	case 50:
		m.Text = "[语音/视频通话]"
	case 37:
		m.Text = "[好友申请]"
	case 49:
		renderApp(m, body, cs)
	case 10000:
		m.Text = systemText(body)
	case 10002:
		m.Text = sysmsgText(body, cs, m.Chat)
	default:
		m.Text = fmt.Sprintf("[消息类型 %d]", m.Type)
	}
}

func renderApp(m *Message, body string, cs *contactSet) {
	var a appMsgXML
	if !parseXML(body, &a) {
		m.Text = "[卡片消息]"
		return
	}
	am := a.AppMsg
	from := firstNonEmpty(a.FromUser, am.Pat.From)
	if m.Sender == "" && from != "" {
		m.Sender = from
		m.IsSelf = from == cs.self
		if m.IsSelf {
			m.SenderName = "我"
		} else {
			m.SenderName = cs.senderName(m.Chat, from)
		}
	}
	st := am.Type
	if st == 0 {
		st = m.SubType
	}
	if k := appKinds[st]; k != "" {
		m.Kind = k
	}
	title := strings.TrimSpace(am.Title)
	ex := &Extra{Title: title, Desc: strings.TrimSpace(am.Des), URL: am.URL, Source: am.Source}
	switch st {
	case 57:
		m.Text = title
		if r := am.Refer; r != nil {
			q := &Quote{Sender: r.ChatUsr, SenderName: r.DisplayName, ServerID: r.SvrID}
			if q.Sender == "" {
				q.Sender = r.FromUsr
			}
			if q.SenderName == "" && q.Sender != "" {
				q.SenderName = cs.senderName(m.Chat, q.Sender)
			}
			q.Text = quoteText(r.Type, r.Content, cs, m.Chat)
			ex.Quote = q
		}
	case 6, 74:
		ex.FileSize = am.Attach.TotalLen
		m.Text = "[文件] " + title
	case 2000:
		m.Text = strings.TrimSpace("[转账] " + am.PayInfo.FeeDesc + " " + am.PayInfo.PayMemo)
	case 2001:
		m.Text = strings.TrimSpace("[红包] " + firstNonEmpty(am.PayInfo.SenderTitle, title))
	case 51:
		m.Text = strings.TrimSpace("[视频号] " + am.Finder.Nickname + " " + firstNonEmpty(am.Finder.Desc, title))
	case 33, 36:
		m.Text = strings.TrimSpace("[小程序] " + firstNonEmpty(am.Source, "") + " " + title)
	case 19, 40:
		m.Text = "[聊天记录] " + title
		if ex.Desc != "" {
			m.Text += "\n" + ex.Desc
		}
	case 62:
		m.Text = title
		if t := am.Pat.Template; t != "" {
			m.Text = placeholder.ReplaceAllStringFunc(t, func(p string) string {
				u := p[2 : len(p)-1]
				if u == cs.self {
					return "我"
				}
				return cs.senderName(m.Chat, u)
			})
		}
	case 5, 4, 3:
		m.Text = "[链接] " + title
		if am.URL != "" {
			m.Text += " " + am.URL
		}
	case 8:
		m.Text = "[表情]"
	default:
		if title != "" {
			m.Text = "[卡片] " + title
		} else {
			m.Text = "[卡片消息]"
		}
	}
	m.Extra = ex
}

func quoteText(t int, content string, cs *contactSet, chat string) string {
	switch t {
	case 1:
		_, rest, _ := splitSenderPrefix(content)
		return rest
	case 49:
		var a appMsgXML
		if parseXML(html.UnescapeString(content), &a) && a.AppMsg.Title != "" {
			return a.AppMsg.Title
		}
		return "[卡片消息]"
	}
	tmp := Message{Type: t, Chat: chat}
	render(&tmp, content, cs)
	return tmp.Text
}

type sysTemplateXML struct {
	Template string `xml:"sysmsgtemplate>content_template>template"`
	Links    []struct {
		Name      string `xml:"name,attr"`
		Separator string `xml:"separator"`
		Members   []struct {
			Username string `xml:"username"`
			Nickname string `xml:"nickname"`
		} `xml:"memberlist>member"`
		Title string `xml:"title"`
	} `xml:"sysmsgtemplate>content_template>link_list>link"`
}

var dollarVar = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)\$`)

func systemText(body string) string {
	if r := replaceRe.FindStringSubmatch(body); r != nil {
		return html.UnescapeString(strings.TrimSpace(r[1]))
	}
	if strings.Contains(body, "<sysmsgtemplate") {
		var t sysTemplateXML
		if parseXML(body, &t) && t.Template != "" {
			vals := map[string]string{}
			for _, l := range t.Links {
				var names []string
				for _, m := range l.Members {
					names = append(names, firstNonEmpty(m.Nickname, m.Username))
				}
				sep := l.Separator
				if sep == "" {
					sep = "、"
				}
				vals[l.Name] = firstNonEmpty(strings.Join(names, sep), l.Title)
			}
			return dollarVar.ReplaceAllStringFunc(t.Template, func(v string) string {
				return vals[v[1:len(v)-1]]
			})
		}
	}
	return strings.TrimSpace(html.UnescapeString(tagRe.ReplaceAllString(body, "")))
}

func sysmsgText(body string, cs *contactSet, chat string) string {
	if r := replaceRe.FindStringSubmatch(body); r != nil {
		return html.UnescapeString(strings.TrimSpace(r[1]))
	}
	if t := templateRe.FindStringSubmatch(body); t != nil {
		return placeholder.ReplaceAllStringFunc(strings.TrimSpace(t[1]), func(p string) string {
			u := p[2 : len(p)-1]
			return "\"" + cs.senderName(chat, u) + "\""
		})
	}
	s := strings.TrimSpace(html.UnescapeString(tagRe.ReplaceAllString(body, " ")))
	if s == "" {
		return "[系统消息]"
	}
	return strings.Join(strings.Fields(s), " ")
}
