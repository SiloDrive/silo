require_relative "test_helper"
require "digest"
require "securerandom"

# The block surface, end to end against a running server. These cover what the
# Go unit tests deliberately cannot: that the HTTP shapes agree with the
# documentation, and that a client which knows only what docs/protocol.md says
# can perform a resumable upload.
class BlocksTest < Minitest::Test
  include SiloTestHelper

  def block_size
    @block_size ||= (client.server_info["block_size"] || 8 * 1024 * 1024)
  end

  def sha1(bytes)
    Digest::SHA1.hexdigest(bytes)
  end

  # Chunk the way docs/protocol.md says to: fixed offsets, SHA-1 of each piece.
  # Nothing here asks the server what the ids should be — that is the property
  # under test.
  def chunk(bytes, size)
    bytes.bytes.each_slice(size).map { |slice| slice.pack("C*") }
  end

  def test_server_advertises_the_surface_and_its_block_size
    info = client.server_info
    assert info.ok?
    assert_includes info["features"], "blocks"
    assert info["block_size"].is_a?(Integer), "block_size must be a number a client can chunk by"
    assert info["block_size"] > 0
  end

  def test_missing_reports_only_what_is_absent
    repo_id = create_test_repo
    present = "held content #{SecureRandom.hex(8)}"
    absent = sha1("never uploaded #{SecureRandom.hex(8)}")

    assert client.put_block(repo_id, sha1(present), present).ok?

    resp = client.missing_blocks(repo_id, [sha1(present), absent])
    assert resp.ok?, resp.to_s
    assert_equal [absent], resp["missing"]
  end

  def test_missing_of_nothing_is_an_empty_array_not_null
    repo_id = create_test_repo
    resp = client.missing_blocks(repo_id, [])
    assert resp.ok?
    assert_equal [], resp["missing"], "null here breaks every client that is not Go"
  end

  def test_a_block_that_does_not_hash_to_its_id_is_refused
    repo_id = create_test_repo
    wrong_id = sha1("one thing")

    resp = client.put_block(repo_id, wrong_id, "a different thing")
    assert_equal 400, resp.status, "the store accepted content under an id that is not its hash"

    # And nothing was published under that name, or a later writer of the same
    # id would skip it as already present and serve the wrong bytes forever.
    assert_equal [wrong_id], client.missing_blocks(repo_id, [wrong_id])["missing"]
  end

  def test_a_block_already_held_answers_204
    repo_id = create_test_repo
    content = "sent twice #{SecureRandom.hex(8)}"

    assert_equal 201, client.put_block(repo_id, sha1(content), content).status
    assert_equal 204, client.put_block(repo_id, sha1(content), content).status
  end

  def test_a_file_uploaded_as_blocks_reads_back_whole
    repo_id = create_test_repo
    # Small blocks would not match the server's chunking, so use its own size
    # for the first block and a short second one — the two cases a file has.
    content = "S" * (block_size + 100)
    blocks = chunk(content, block_size)
    ids = blocks.map { |b| sha1(b) }

    missing = client.missing_blocks(repo_id, ids)["missing"]
    missing.each do |id|
      body = blocks[ids.index(id)]
      assert client.put_block(repo_id, id, body).ok?
    end

    created = client.create_from_blocks(repo_id, "/big.bin", ids)
    assert_equal 201, created.status, created.to_s
    assert created.header("ETag"), "a write returns the new ETag so a client need not re-read"

    got = client.get(client.entries_url(repo_id, "/big.bin"))
    assert got.ok?
    assert_equal content.bytesize, got.body.bytesize
    assert_equal content, got.body
  end

  # The property that makes an interrupted upload resumable: asking again is
  # the whole recovery, and the second answer is shorter.
  def test_re_asking_after_uploading_returns_a_shorter_list
    repo_id = create_test_repo
    parts = 3.times.map { "part #{SecureRandom.hex(8)}" }
    ids = parts.map { |p| sha1(p) }

    assert_equal ids, client.missing_blocks(repo_id, ids)["missing"]

    client.put_block(repo_id, ids[0], parts[0])
    assert_equal ids[1..], client.missing_blocks(repo_id, ids)["missing"]

    parts[1..].each_with_index { |p, i| client.put_block(repo_id, ids[i + 1], p) }
    assert_equal [], client.missing_blocks(repo_id, ids)["missing"]
  end

  def test_creating_from_blocks_that_are_not_there_is_424_and_names_them
    repo_id = create_test_repo
    absent = sha1("never sent #{SecureRandom.hex(8)}")

    resp = client.create_from_blocks(repo_id, "/nope.bin", [absent])
    assert_equal 424, resp.status, "a commit naming absent blocks must not read as a client error"
    assert_equal [absent], resp["missing"], "the reply has to say which, or the fix is a re-upload of everything"
  end

  def test_nothing_exists_until_the_last_call
    repo_id = create_test_repo
    content = "uploaded but never committed #{SecureRandom.hex(8)}"

    assert client.put_block(repo_id, sha1(content), content).ok?

    listing = client.list_dir(repo_id, "/")
    assert listing.ok?
    assert_equal [], listing.json, "uploading blocks created something in the tree"
  end
end
