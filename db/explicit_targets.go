package db

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type TargetEndpoint struct {
	Host string
	Port int
}

func ParseTargetEndpoint(raw string) (TargetEndpoint, bool) { return parseTargetEndpoint(raw) }

var explicitTargetLiteral = regexp.MustCompile(`(?i)(?:https?://)?(?:\[[0-9a-f:]+\]|(?:[a-z0-9-]+\.)+[a-z0-9-]+|(?:\d{1,3}\.){3}\d{1,3})(?::\d{1,5})?`)
var forbiddenDiscoveryIntent = regexp.MustCompile(`(?i)端口扫描|扫端口|探测端口|端口探测|全端口|(?:扫描|探测|枚举|爆破)[^。；;\n]{0,40}(?:端口|子域)|(?:端口|子域)[^。；;\n]{0,40}(?:扫描|探测|枚举|爆破)|(?:其他|更多|全部|所有)(?:端口|子域)|子域名探测|子域名枚举|域名爆破|枚举子域名|port[ -]?scan|subdomain[ -]?(?:scan|enumerat|bruteforce)|nmap|masscan|rustscan|zmap|subfinder|amass|dnsx`)
var mentionedIntentPort = regexp.MustCompile(`(?:端口[\s:：]*([0-9]{1,5})|([0-9]{1,5})[\s]*端口)`)

func defaultTargetPort(host, scheme string) int {
	if scheme == "http" || (scheme == "" && net.ParseIP(host) != nil) {
		return 80
	}
	return 443
}

func parseTargetEndpoint(raw string) (TargetEndpoint, bool) {
	raw = strings.TrimRight(strings.TrimSpace(raw), ".,;!?)]}，；。")
	if raw == "" || strings.HasPrefix(raw, "*.") {
		return TargetEndpoint{}, false
	}
	address := raw
	if !strings.Contains(address, "://") {
		address = "//" + address
	}
	u, err := url.Parse(address)
	if err != nil || u.Hostname() == "" {
		return TargetEndpoint{}, false
	}
	host, err := NormalizeAgentHost(u.Hostname(), true)
	if err != nil {
		return TargetEndpoint{}, false
	}
	port := defaultTargetPort(host, strings.ToLower(u.Scheme))
	if u.Port() != "" {
		port, err = strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return TargetEndpoint{}, false
		}
	}
	return TargetEndpoint{Host: host, Port: port}, true
}

func explicitTargetsIn(text string) []TargetEndpoint {
	seen := map[TargetEndpoint]bool{}
	var result []TargetEndpoint
	for _, match := range explicitTargetLiteral.FindAllStringIndex(text, -1) {
		if match[1] < len(text) && (text[match[1]] == ':' || (text[match[1]] >= '0' && text[match[1]] <= '9')) {
			continue
		}
		literal := text[match[0]:match[1]]
		if target, ok := parseTargetEndpoint(literal); ok && !seen[target] {
			seen[target] = true
			result = append(result, target)
		}
	}
	return result
}

// The task's verified description grants and operator-added assets are the
// only sources of executable host:port pairs under explicit_targets.
func (s *AssetStore) explicitTargetPorts(taskID int64) (map[TargetEndpoint]bool, error) {
	allowed := map[TargetEndpoint]bool{}
	rows, err := s.query(`SELECT value,evidence FROM task_asset_grants
WHERE task_id=$1 AND kind='host' AND source='description'`, taskID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var host, evidence string
		if err := rows.Scan(&host, &evidence); err != nil {
			rows.Close()
			return nil, err
		}
		for _, target := range explicitTargetsIn(evidence) {
			if target.Host == host {
				allowed[target] = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	rows, err = s.query(`SELECT task_asset_host(a),COALESCE(a.url,''),COALESCE(a.port,0)
FROM task_asset_links l JOIN assets a ON a.id=l.asset_id
WHERE l.task_id=$1 AND l.source IN ('manual','direct','company','api')
AND l.approval_state='approved' AND a.type IN ('root_domain','subdomain','ip','service','endpoint')`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var host, rawURL string
		var port int
		if err := rows.Scan(&host, &rawURL, &port); err != nil {
			return nil, err
		}
		if rawURL != "" {
			if target, ok := parseTargetEndpoint(rawURL); ok && target.Host == host {
				allowed[target] = true
			}
			continue
		}
		if port == 0 {
			port = defaultTargetPort(host, "")
		}
		if port > 0 && port <= 65535 {
			allowed[TargetEndpoint{Host: host, Port: port}] = true
		}
	}
	return allowed, rows.Err()
}

func (s *AssetStore) ValidateExplicitTarget(taskID int64, host string, port int) error {
	template, err := s.TaskApprovalTemplate(taskID)
	if err != nil {
		return err
	}
	if template != "explicit_targets" {
		return nil
	}
	host, err = NormalizeAgentHost(host, true)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("%w: invalid target host or port", ErrTaskAssetInvalid)
	}
	allowed, err := s.explicitTargetPorts(taskID)
	if err != nil {
		return err
	}
	if !allowed[TargetEndpoint{Host: host, Port: port}] {
		return fmt.Errorf("%w: %s:%d 不在用户明确给定的目标端口内", ErrTaskAssetNotApproved, host, port)
	}
	return nil
}

func (s *AssetStore) ValidateExplicitIntent(taskID int64, summary string) error {
	template, err := s.TaskApprovalTemplate(taskID)
	if err != nil {
		return err
	}
	if template != "explicit_targets" {
		return nil
	}
	if forbiddenDiscoveryIntent.MatchString(summary) {
		return fmt.Errorf("%w: 当前审批模板不允许端口扫描或子域名探测意图", ErrTaskAssetNotApproved)
	}
	targets := explicitTargetsIn(summary)
	mentionedPorts := map[int]bool{}
	for _, match := range mentionedIntentPort.FindAllStringSubmatch(summary, -1) {
		rawPort := match[1]
		if rawPort == "" {
			rawPort = match[2]
		}
		port, err := strconv.Atoi(rawPort)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("%w: 无效意图端口", ErrTaskAssetInvalid)
		}
		mentionedPorts[port] = true
	}
	for _, target := range targets {
		explicitPort := strings.Contains(summary, target.Host+":"+strconv.Itoa(target.Port)) || strings.Contains(summary, "["+target.Host+"]:"+strconv.Itoa(target.Port))
		if len(mentionedPorts) == 0 || explicitPort {
			if err := s.ValidateExplicitTarget(taskID, target.Host, target.Port); err != nil {
				return err
			}
		}
		for port := range mentionedPorts {
			if err := s.ValidateExplicitTarget(taskID, target.Host, port); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *AssetStore) ValidateExplicitIntentPayload(taskID int64, payload []byte) error {
	var intent struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(payload, &intent); err != nil {
		return err
	}
	return s.ValidateExplicitIntent(taskID, intent.Summary)
}

func (s *AssetStore) ValidateExplicitAssets(taskID int64, ids []int64) error {
	template, err := s.TaskApprovalTemplate(taskID)
	if err != nil || template != "explicit_targets" || len(ids) == 0 {
		return err
	}
	allowed, err := s.explicitTargetPorts(taskID)
	if err != nil {
		return err
	}
	rows, err := s.query(`SELECT task_asset_host(a),COALESCE(a.url,''),COALESCE(a.port,0)
FROM assets a WHERE a.id=ANY($1::bigint[])`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var host, rawURL string
		var port int
		if err := rows.Scan(&host, &rawURL, &port); err != nil {
			return err
		}
		if rawURL != "" {
			if target, ok := parseTargetEndpoint(rawURL); ok {
				port = target.Port
			}
		}
		if port > 0 {
			if !allowed[TargetEndpoint{Host: host, Port: port}] {
				return fmt.Errorf("%w: 资产 %s:%d 不在用户给定的目标端口内", ErrTaskAssetNotApproved, host, port)
			}
			continue
		}
		matched := false
		for target := range allowed {
			if target.Host == host {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%w: 资产 %s 不在用户给定的目标内", ErrTaskAssetNotApproved, host)
		}
	}
	return rows.Err()
}
