require_relative "test_helper"
require "securerandom"

# Pagination, end to end. What matters here is the contract rather than the
# arithmetic: the body shape did not change, the next page is a Link header,
# and the anchor does not appear until a client has seen everything.
class PaginationTest < Minitest::Test
  include SiloTestHelper

  def next_link(resp)
    link = resp.header("Link")
    return nil unless link
    link[/<([^>]+)>\s*;\s*rel="?next"?/, 1]
  end

  def test_server_advertises_pagination
    assert_includes client.server_info["features"], "pagination"
  end

  def test_a_listing_without_limit_is_whole_and_unchanged_in_shape
    repo_id = create_test_repo
    3.times { |i| client.mkdir(repo_id, "/d#{i}") }

    resp = client.list_dir(repo_id, "/")
    assert resp.ok?
    assert_kind_of Array, resp.json, "the unpaged body is still a bare array"
    assert_equal 3, resp.json.length
    assert_nil next_link(resp), "an unpaged response must not advertise a next page"
  end

  def test_a_listing_pages_through_a_link_header
    repo_id = create_test_repo
    5.times { |i| client.mkdir(repo_id, "/d#{i}") }

    seen = []
    resp = client.list_dir(repo_id, "/", "limit=2")
    pages = 0
    loop do
      assert resp.ok?, resp.to_s
      assert_kind_of Array, resp.json, "a paged body is still an array"
      seen.concat(resp.json.map { |e| e["name"] })
      pages += 1
      link = next_link(resp)
      break unless link
      assert pages < 10, "pagination did not terminate"
      resp = client.get(link)
    end

    assert_equal 3, pages, "5 entries at 2 per page is 3 pages"
    assert_equal 5, seen.length
    assert_equal seen.uniq.length, seen.length, "an entry appeared on two pages"
  end

  def test_changes_withholds_the_anchor_until_the_last_page
    repo_id = create_test_repo
    before = client.list_repos.json.find { |r| r["id"] == repo_id }["head_commit_id"]
    4.times { |i| client.mkdir(repo_id, "/c#{i}") }

    resp = client.changes(repo_id, before, "limit=2")
    assert resp.ok?, resp.to_s
    assert next_link(resp), "there is more to come, so there must be a next link"
    assert_nil resp["anchor"], "recording the anchor here would skip everything unread"

    total = resp["changes"].length
    until (link = next_link(resp)).nil?
      resp = client.get(link)
      assert resp.ok?, resp.to_s
      total += resp["changes"].length
    end

    assert_equal 4, total
    assert resp["anchor"], "the last page carries the anchor"

    # And the anchor really is where the client now stands.
    assert_equal [], client.changes(repo_id, resp["anchor"])["changes"]
  end

  def test_a_bad_limit_is_refused_rather_than_clamped
    repo_id = create_test_repo
    ["limit=0", "limit=-1", "limit=many", "limit=10001"].each do |q|
      assert_equal 400, client.list_dir(repo_id, "/", q).status, "?#{q} was accepted"
    end
  end

  def test_a_cursor_this_server_did_not_issue_is_refused
    repo_id = create_test_repo
    assert_equal 400, client.list_dir(repo_id, "/", "cursor=not-a-cursor").status
  end
end
