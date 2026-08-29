require_relative "test_helper"
require "digest"
require "securerandom"

# The batch surface, end to end. The Go tests cover the tree operations; these
# cover the contract a client reads about — one commit, all or nothing, and a
# failure that says which operation stopped it.
class BatchTest < Minitest::Test
  include SiloTestHelper

  # SHA-256, sixty-four hex characters. store-v2 changed the address hash, and
  # this file computed SHA-1 for long enough afterwards that four of its tests
  # were failing against a route whose regex simply does not match a forty-hex
  # id. See chunks_test.rb for the surface itself.
  def chunk_id(bytes)
    Digest::SHA256.hexdigest(bytes)
  end

  def test_server_advertises_batch
    assert_includes client.server_info["features"], "batch"
  end

  def test_many_operations_land_as_one_commit
    library_id = create_test_library
    before = client.list_libraries.json.find { |r| r["id"] == library_id }["head_commit_id"]

    contents = 3.times.map { "file #{SecureRandom.hex(8)}" }
    ids = contents.map { |c| chunk_id(c) }
    contents.each_with_index { |c, i| client.put_chunk(library_id, ids[i], c) }

    ops = [{ op: "mkdir", path: "/reports" }]
    ids.each_with_index { |id, i| ops << { op: "create", path: "/reports/f#{i}.txt", chunks: [id] } }

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
    # /reports is reported in its own right, alongside the files inside it.
    # This file used to assert the opposite, from a rule that predates store-v2
    # — a directory arriving with content appeared solely as its contents — and
    # the diff now reports a subtree in full, pinned on the Go side as
    # objmgr.TestDiffReportsASubtreeInFull.
    changed = client.changes(library_id, before)
    assert changed.ok?, changed.to_s
    paths = changed["changes"].map { |c| c["path"] }.sort
    assert_equal ["/reports", "/reports/f0.txt", "/reports/f1.txt", "/reports/f2.txt"], paths

    dirs = changed["changes"].select { |c| c["is_dir"] }.map { |c| c["path"] }
    assert_equal ["/reports"], dirs, "the directory row has to be marked as one"
  end

  def test_an_operation_sees_the_ones_before_it
    library_id = create_test_library
    content = "nested #{SecureRandom.hex(8)}"
    client.put_chunk(library_id, chunk_id(content), content)

    resp = client.batch(library_id, [
      { op: "mkdir", path: "/a" },
      { op: "mkdir", path: "/a/b" },
      { op: "create", path: "/a/b/c.txt", chunks: [chunk_id(content)] }
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

  # mkdir of an existing directory is a conflict, and the batch it is in writes
  # nothing. This file asserted the opposite -- that it was quietly tolerated --
  # which stopped being true under store-v2; the Go side pins it as
  # TestBatchMkdirOfAnExistingDirectoryIsRefusedOnStoreV2.
  #
  # All-or-nothing is the part worth measuring from out here: the second
  # operation is perfectly good, and it must not survive the first one failing.
  def test_mkdir_of_an_existing_directory_is_a_conflict
    library_id = create_test_library
    assert client.batch(library_id, [{ op: "mkdir", path: "/twice" }]).ok?

    resp = client.batch(library_id, [{ op: "mkdir", path: "/twice" }, { op: "mkdir", path: "/other" }])
    assert_equal 409, resp.status, resp.to_s
    assert_equal 0, resp["index"], "the reply has to name the operation that stopped it"

    refute client.get(client.entries_url(library_id, "/other")).ok?,
      "an operation after the failure was applied anyway"
  end

  # A batch whose operations all land where they started leaves the tree
  # identical, and an identical tree mints no commit -- reporting the head the
  # request loaded is more honest than a commit that says nothing happened.
  #
  # A repeated mkdir used to be how this file reached that state, and it is now
  # a 409. A move out and back is the remaining way to ask a batch to do real
  # work and arrive nowhere.
  def test_a_batch_that_changes_nothing_mints_no_commit
    library_id = create_test_library
    assert client.batch(library_id, [{ op: "mkdir", path: "/here" }]).ok?
    head = client.list_libraries.json.find { |r| r["id"] == library_id }["head_commit_id"]

    resp = client.batch(library_id, [
      { op: "move", path: "/here", to: "/there" },
      { op: "move", path: "/there", to: "/here" }
    ])
    assert resp.ok?, resp.to_s
    refute resp["changed"], "an unchanged tree should not report a change"
    assert_equal head, resp["commit_id"], "an unchanged tree should not mint a commit"
    assert_equal ["here"], client.list_dir(library_id, "/").json.map { |e| e["name"] }
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
