require_relative "test_helper"
require "securerandom"

class TokensTest < Minitest::Test
  include SiloTestHelper

  def test_create_sync_token
    repo_id = create_test_repo

    resp = client.create_sync_token(repo_id)
    assert resp.ok?, "Sync token creation failed: #{resp}"
    assert resp["token"], "Expected token in response"
    assert_equal 40, resp["token"].length, "Sync token should be 40-char hex (SHA1)"
  end

  def test_sync_token_works_for_head_commit
    repo_id = create_test_repo

    sync_token = client.create_sync_token(repo_id)["token"]

    resp = client.get_head_commit(repo_id, sync_token)
    assert resp.ok?, "HEAD commit query failed: #{resp}"
    assert resp["head_commit_id"], "Expected head_commit_id"
    assert_equal 40, resp["head_commit_id"].length, "Commit ID should be 40-char hex"
  end

  # 403 rather than 404, and deliberately so: a library you cannot see and a
  # library that does not exist answer the same thing here, or this endpoint
  # becomes a way to probe for valid library ids. The test asked for 404 from
  # before that was decided — see docs/responses.md.
  def test_sync_token_for_nonexistent_repo_is_indistinguishable_from_one_you_cannot_see
    resp = client.create_sync_token("00000000-0000-0000-0000-000000000000")
    assert_equal 403, resp.status
  end

  def test_create_access_token
    repo_id = create_test_repo

    resp = client.create_access_token(repo_id: repo_id, op: "download")
    assert resp.ok?, "Access token creation failed: #{resp}"
    assert resp["token"], "Expected token in response"
  end

  def test_access_token_missing_fields
    resp = client.post("/api/silo/v1/access-tokens", { op: "download" })
    assert_equal 400, resp.status

    resp = client.post("/api/silo/v1/access-tokens", { repo_id: "some-id" })
    assert_equal 400, resp.status
  end

  def test_head_commit_is_consistent
    repo_id = create_test_repo
    sync_token = client.create_sync_token(repo_id)["token"]

    # Two queries should return the same HEAD (no changes made)
    head1 = client.get_head_commit(repo_id, sync_token)["head_commit_id"]
    head2 = client.get_head_commit(repo_id, sync_token)["head_commit_id"]
    assert_equal head1, head2, "HEAD should be stable when no changes are made"
  end
end
