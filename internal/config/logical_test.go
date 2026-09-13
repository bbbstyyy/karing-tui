package config

import "testing"

func TestParseLogicalExpr(t *testing.T) {
	mode, conds, err := ParseLogicalExpr("rule_set=geosite-cn && !ip_cidr=10.0.0.0/8,192.168.0.0/16")
	if err != nil {
		t.Fatalf("ParseLogicalExpr: %v", err)
	}
	if mode != "and" {
		t.Errorf("mode = %q, 期望 and", mode)
	}
	if len(conds) != 2 {
		t.Fatalf("条件数 = %d, 期望 2", len(conds))
	}
	if conds[0].Type != "rule_set" || conds[0].Value != "geosite-cn" || conds[0].Invert {
		t.Errorf("条件 0 不符: %+v", conds[0])
	}
	if conds[1].Type != "ip_cidr" || conds[1].Value != "10.0.0.0/8,192.168.0.0/16" || !conds[1].Invert {
		t.Errorf("条件 1 不符: %+v", conds[1])
	}

	mode, conds, err = ParseLogicalExpr("domain_suffix=openai.com || domain_keyword=claude")
	if err != nil {
		t.Fatalf("ParseLogicalExpr(or): %v", err)
	}
	if mode != "or" || len(conds) != 2 {
		t.Errorf("or 表达式解析不符: mode=%q, %d 条件", mode, len(conds))
	}

	// 单条件默认 and
	mode, conds, err = ParseLogicalExpr("geosite=google")
	if err != nil || mode != "and" || len(conds) != 1 {
		t.Errorf("单条件解析不符: mode=%q, %d 条件, err=%v", mode, len(conds), err)
	}

	// 类型大小写归一
	_, _, err = ParseLogicalExpr("RULE_SET=geosite-cn")
	if err != nil {
		t.Errorf("大写类型应归一: %v", err)
	}
}

func TestParseLogicalExprErrors(t *testing.T) {
	cases := []string{
		"",                             // 空
		"rule_set=",                    // 缺值
		"rule_set=a &&",                // 空条件
		"rule_set=a && ip_cidr=b || c", // 混用 && 与 ||
		"final=x",                      // final 不可作条件
		"nosuch=x",                     // 未知类型
		"rule_set a",                   // 缺 =
	}
	for _, expr := range cases {
		if _, _, err := ParseLogicalExpr(expr); err == nil {
			t.Errorf("表达式 %q 应返回错误", expr)
		}
	}
}

func TestFormatLogicalExprRoundTrip(t *testing.T) {
	expr := "rule_set=geosite-cn && !ip_cidr=10.0.0.0/8"
	mode, conds, err := ParseLogicalExpr(expr)
	if err != nil {
		t.Fatalf("ParseLogicalExpr: %v", err)
	}
	if got := FormatLogicalExpr(mode, conds); got != expr {
		t.Errorf("格式化回显不符: got %q, want %q", got, expr)
	}

	orExpr := "domain_suffix=a.com || domain_suffix=b.com"
	mode, conds, err = ParseLogicalExpr(orExpr)
	if err != nil {
		t.Fatalf("ParseLogicalExpr(or): %v", err)
	}
	if got := FormatLogicalExpr(mode, conds); got != orExpr {
		t.Errorf("or 格式化回显不符: got %q, want %q", got, orExpr)
	}
}
