require_relative "test_helper"
require "securerandom"

class ReposTest < Minitest::Test
  include SiloTestHelper

  def test_create_repo
    resp = client.create_repo("My Test Library")
    assert resp.ok?, "Create failed: #{resp}"
    assert resp["id"], "Expected id in response"
    assert_equal "My Test Library", resp["name"]

    @test_repos = [resp["id"]]
  end

  def test_create_repo_returns_201
    resp = client.create_repo("Status Check")
    assert_equal 201, resp.status

    @test_repos = [resp["id"]]
  end

  def test_create_repo_missing_name
    resp = client.post("/api/silo/v1/repos", { name: "" })
    assert_equal 400, resp.status
  end

  def test_list_repos_includes_created
    repo_id = create_test_repo("Listed Repo")

    resp = client.list_repos
    assert resp.ok?, "List failed: #{resp}"

    repos = resp.json
    assert_kind_of Array, repos
    assert repos.any? { |r| r["id"] == repo_id }, "Created repo not found in list"
  end

  def test_delete_repo
    repo_id = create_test_repo("To Delete")

    resp = client.delete_repo(repo_id)
    assert resp.ok?, "Delete failed: #{resp}"

    # Should be gone from the list
    repos = client.list_repos.json
    refute repos.any? { |r| r["id"] == repo_id }, "Deleted repo still in list"

    # Remove from cleanup list since we already deleted it
    @test_repos.delete(repo_id)
  end

  def test_delete_nonexistent_repo
    resp = client.delete_repo("00000000-0000-0000-0000-000000000000")
    assert_equal 404, resp.status
  end

  def test_create_and_delete_multiple
    ids = 3.times.map { |i| create_test_repo("Batch #{i}") }

    repos = client.list_repos.json
    ids.each do |id|
      assert repos.any? { |r| r["id"] == id }, "Repo #{id} not in list"
    end

    ids.each do |id|
      resp = client.delete_repo(id)
      assert resp.ok?, "Delete #{id} failed: #{resp}"
    end

    repos = client.list_repos.json
    ids.each do |id|
      refute repos.any? { |r| r["id"] == id }, "Repo #{id} still in list after delete"
    end

    @test_repos = []
  end

  def test_repo_has_expected_fields
    repo_id = create_test_repo("Field Check")

    repos = client.list_repos.json
    repo = repos.find { |r| r["id"] == repo_id }

    assert repo, "Repo not found"
    assert_includes repo.keys, "id"
    assert_includes repo.keys, "name"
    assert_includes repo.keys, "update_time"
    assert_includes repo.keys, "encrypted"
    assert_equal "Field Check", repo["name"]
    assert_equal false, repo["encrypted"]
  end
end
