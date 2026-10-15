import assert from "node:assert/strict";
import test from "node:test";
import { activeMention, mentionSearch, mentionToken, selectedMentions } from "./chat-mentions.ts";

test("mention trigger supports cursor placement without hijacking email", () => {
  assert.equal(activeMention("user@example.com", 16), null);
  assert.equal(activeMention("selected @[finding#1 X]", 12), null);
  assert.deepEqual(activeMention("look at @finding ", 16), { start: 8, end: 16, query: "finding" });
  assert.equal(activeMention("@finding\nnext line", 14), null);
});

test("categories, aliases, IP and keyword search", () => {
  assert.equal(mentionSearch("").categories.length, 9);
  assert.equal(mentionSearch("fi").categories[0].kind, "finding");
  assert.equal(mentionSearch("finding").kind, "finding");
  assert.equal(mentionSearch("findingSQLi").query, "SQLi");
  assert.equal(mentionSearch("ip 192.0.2.1").kind, "ip");
  assert.equal(mentionSearch("endpoint GET /api").query, "GET /api");
  assert.equal(mentionSearch("acme.com").kind, "");
});

test("mention token round-trips through the ASCII alias form", () => {
  const token = mentionToken({ kind: "finding", id: 7, label: "SQL インジェクション", description: "" });
  assert.equal(token, "@[finding#7 SQL インジェクション]");
  const [sel] = selectedMentions(token);
  assert.equal(sel.label, "finding #7 · SQL インジェクション");
});

test("legacy Chinese tokens from pre-migration history still parse", () => {
  const [sel] = selectedMentions("查看@漏洞 后面的文字 @[漏洞#1 X]");
  assert.equal(sel.label, "漏洞 #1 · X");
  assert.equal(mentionSearch("finding").kind, "finding");
});
