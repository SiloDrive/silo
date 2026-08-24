require_relative "test_helper"
require "digest"
require "securerandom"

# The batch surface, end to end. The Go tests cover the tree operations; these
# cover the contract a client reads about — one commit, all or nothing, and a
# failure that says which operation stopped it.
class BatchTest < Minitest::Test
  include SiloTestHelper

  def sha1(bytes)
    Digest::SHA1.hexdigest(bytes)
  end

  def test_server_advertises_batch
    assert_includes client.server_info["features"], "batch"
  end

  def test_many_operations_land_as_one_commit
    library_id = create_test_library
    before = client.list_libraries.json.find { |r| r["id"] == library_id }["head_commit_id"]

    contents = 3.times.map { "file #{SecureRandom.hex(8)}" }
    ids = contents.map { |c| sha1(c) }
    contents.each_with_index { |c, i| client.put_block(library_id, ids[i], c) }

    ops = [{ op: "mkdir", path: "/reports" }]
    ids.each_with_index { |id, i| ops << { op: "create", path: "/reports/f#{i}.txt", blocks: [id] } }

    resp = client.batch(library_id, ops)
    assert resp.ok?, resp.to_s
    assert_equal ops.length, resp["ops"]
    assert resp["changed"]
    refute_equal before, resp["commit_id"], "the batch did not advance the head"

    listing = client.list_dir(library_id, "/reports")
    assert_equal 3, listing.json.length

    # One commit for the lot, which is the whole reason this endpoint exists:
    # everything the batch did shows up in a single diff from the anchor the
    # client held before it.
    #
    # /reports itself is absent on purpose. The delta feed reports a directory
    # in its own right only when it is empty — one that arrives with content
    # appears solely as the paths inside it — which is why an applier needs
    # mkdir -p semantics. See docs/sync-design.md.
    changed = client.changes(library_id, before)
    assert changed.ok?, changed.to_s
    paths = changed["changes"].map { |c| c["path"] }.sort
    assert_equal ["/reports/f0.txt", "/reports/f1.txt", "/reports/f2.txt"], paths
  end

  def test_an_operation_sees_the_ones_before_it
    library_id = create_test_library
    content = "nested #{SecureRandom.hex(8)}"
    client.put_block(library_id, sha1(content), content)

    resp = client.batch(library_id, [
      { op: "mkdir", path: "/a" },
      { op: "mkdir", path: "/a/b" },
      { op: "create", path: "/a/b/c.txt", blocks: [sha1(content)] }
    ])
    assert resp.ok?, resp.to_s
    assert client.get(client.entries_url(library_id, "/a/b/c.txt")).ok?
  end

  def test_a_failure_writes_nothing_and_names_the_operation
    library_id = create_test_library
    assert client.mkdir(library_id, "/keep").ok?

    resp = client.batch(library_id, [
      { op: "mkdir", path: "/first" },
      { op: "mkdir", path: "/second" },
      { op: "delete", path: "/not-here" }
    ])
    assert_equal 404, resp.status, resp.to_s
    assert_equal 2, resp["index"], "the reply must say which operation stopped it"
    assert_equal "delete", resp["op"]

    # Nothing from before the failure survived.
    names = client.list_dir(library_id, "/").json.map { |e| e["name"] }
    assert_equal ["keep"], names, "a failed batch left part of itself behind"
  end

  def test_mkdir_of_an_existing_directory_is_not_a_failure
    library_id = create_test_library
    assert client.batch(library_id, [{ op: "mkdir", path: "/twice" }]).ok?

    resp = client.batch(library_id, [{ op: "mkdir", path: "/twice" }, { op: "mkdir", path: "/other" }])
    assert resp.ok?, resp.to_s
    assert client.get(client.entries_url(library_id, "/other")).ok?
  end

  def test_a_batch_that_changes_nothing_mints_no_commit
    library_id = create_test_library
    assert client.batch(library_id, [{ op: "mkdir", path: "/already" }]).ok?
    head = client.list_libraries.json.find { |r| r["id"] == library_id }["head_commit_id"]

    resp = client.batch(library_id, [{ op: "mkdir", path: "/already" }])
    assert resp.ok?, resp.to_s
    refute resp["changed"], "an unchanged tree should not report a change"
    assert_equal head, resp["commit_id"], "an unchanged tree should not mint a commit"
  end

  def test_if_match_on_the_library_root
    library_id = create_test_library
    root_etag = client.list_dir(library_id, "/").header("ETag")
    assert root_etag, "the root listing carries the ETag a batch preconditions on"

    assert client.batch(library_id, [{ op: "mkdir", path: "/one" }], if_match: root_etag).ok?

    # The same precondition a second time is stale: the library moved.
    resp = client.batch(library_id, [{ op: "mkdir", path: "/two" }], if_match: root_etag)
    assert_equal 412, resp.status, "a stale If-Match must not apply"
    assert_equal ["one"], client.list_dir(library_id, "/").json.map { |e| e["name"] }
  end

  def test_an_empty_batch_is_refused
    library_id = create_test_library
    assert_equal 400, client.batch(library_id, []).status
  end

  def test_an_unknown_operation_is_refused
    library_id = create_test_library
    resp = client.batch(library_id, [{ op: "chmod", path: "/x" }])
    assert_equal 400, resp.status
    assert_equal 0, resp["index"]
  end
end
