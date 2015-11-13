var FileSize = React.createClass({
	render: function () {
		var size = this.props.size;
		var units = ['bytes', 'KB', 'MB', 'GB'];
		var i = 0;
		while (size > 1024 && i < (units.length-1)) {
			i++;
			size = size / 1024;
		}
		size = Math.round(size * 10) / 10;
		return <span>{size} {units[i]}</span>;
	}
});

export default FileSize;
